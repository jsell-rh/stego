package service

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// ReachTimeout bounds the check that SpiceDB answers, which New makes before it gives the service.
const ReachTimeout = 10 * time.Second

// Parts are what varies with the declaration of one service, which the generated code gives New
// (ST-SUP-RP14-LC02): names and the constructors of the service's own component packages.
type Parts struct {
	// Service is the name of the service, and DeclarationVersion the SHA-256 of its declaration, which each audit row
	// carries.
	Service, DeclarationVersion string
	// SubjectType is the subject type of the schema that a verified identity maps to, and TenantClaim the dotted path
	// of the claim that names the caller's tenant.
	SubjectType, TenantClaim string
	// ProblemType and ProblemCode are the problem base and the problem code prefix of a refused credential.
	ProblemType, ProblemCode string
	// TenantConfig gives the connection configuration of the PostgreSQL adapter for a connection string, with its
	// verified TLS policy (ST-SUP-RP04-LC09).
	TenantConfig func(dsn string) (tenant.Config, error)
	// NewQueue gives the outbox queue of the runtime role's pool, with the payload limits of the destinations.
	NewQueue func(db *sql.DB) (*outbox.Queue, error)
	// AuditQueue gives the audit enqueuer on the outbox queue, which the audit writer takes.
	AuditQueue func(queue *outbox.Queue) (audit.Enqueuer, error)
	// NewAuthz gives the authorization runtime of the service over the decision provider.
	NewAuthz func(db *sql.DB, provider authz.Decision, queue *outbox.Queue) (*authz.Runtime, error)
	// AuditSettings are the audit settings of the service.
	AuditSettings audit.Settings
	// Commands are the entities of the declared commands, and Collections those of the generated collection
	// commands: each set that is not empty gets a command runtime of its own, since an entity registers with one
	// runtime only.
	Commands, Collections []command.Declared
	// Workers are the parts of the worker and controller modes, which Start runs (ST-SUP-RP14-LC04).
	Workers WorkerParts
	// OwnerRole is the owner role of the service, which the migration job runs as and which must own each table of
	// the schema manifest and each trigger function (ST-SUP-RP14-LC06).
	OwnerRole string
	// Schema is the schema manifest of the tables of the declaration, which the PostgreSQL adapter emits
	// (ST-SUP-RC01-OC12). New adds the manifest of the audit record to it, for the anchor period of Workers
	// (ST-SUP-RP14-LC06).
	Schema manifest.Manifest
}

func (p Parts) check() error {
	switch {
	case p.Service == "" || p.DeclarationVersion == "":
		return errors.New("service: the parts need the service name and the declaration version")
	case p.SubjectType == "" || p.TenantClaim == "" || p.ProblemCode == "":
		return errors.New("service: the parts need the subject type, the tenant claim and the problem code")
	case p.OwnerRole == "":
		return errors.New("service: the parts need the owner role, which owns the tables of the schema manifest")
	case p.TenantConfig == nil || p.NewQueue == nil || p.AuditQueue == nil || p.NewAuthz == nil:
		return errors.New("service: the parts need the adapter's connection configuration, the outbox queue, the audit queue and the authorization runtime")
	}
	return nil
}

// Service holds the runtime library objects of one running service, which New gives.
type Service struct {
	parts         Parts
	provider      *authz.SpiceDB
	pool, auditDB *sql.DB
	keys          *authn.KeySet
	verifier      *authn.Verifier
	spice         *grpc.ClientConn
	telemetry     *otel.Runtime
	probes        *probe.Runtime
	queue         *outbox.Queue
	authz         *authz.Runtime
	writer        *audit.Writer
	commands      *command.Runtime
	collections   *command.Runtime
	queries       *query.Runtime
}

// New wires the runtimes of one service from its settings and its declared parts (ST-SUP-RP14-LC02): it opens the
// main pool of the runtime role through tenant.Open with the adapter's connection configuration, refuses a database
// that does not hold the schema manifest (ST-SUP-RP14-LC06), opens the audit pool,
// connects to SpiceDB with the preshared key and checks that it answers, starts the telemetry and the probes, and
// gives the verifier, the authorization runtime, the audit writer, the outbox queue and the command and query
// runtimes. It refuses settings or parts that lack a part, and closes what it opened when a step fails.
func New(ctx context.Context, s Settings, p Parts) (_ *Service, err error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	svc := &Service{parts: p}
	defer func() {
		// What a failed step left open is closed.
		if err != nil {
			_ = svc.Close()
		}
	}()
	pools, err := p.TenantConfig(s.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if svc.pool, err = tenant.Open(ctx, pools); err != nil {
		return nil, err
	}
	if err := checkSchema(ctx, svc.pool, p.Schema, p.OwnerRole, p.Workers.AnchorPeriod, p.Workers.RetentionMonths); err != nil {
		return nil, err
	}
	if svc.auditDB, err = tenant.Open(ctx, pools); err != nil {
		return nil, err
	}
	if svc.queue, err = p.NewQueue(svc.pool); err != nil {
		return nil, err
	}
	auditQueue, err := p.AuditQueue(svc.queue)
	if err != nil {
		return nil, err
	}
	if svc.writer, err = audit.NewWriter(auditQueue, nil); err != nil {
		return nil, err
	}
	if svc.spice, err = DialSpiceDB(s.SpiceDB); err != nil {
		return nil, err
	}
	if err := reach(ctx, svc.spice); err != nil {
		return nil, err
	}
	if svc.provider, err = authz.NewSpiceDB(svc.spice); err != nil {
		return nil, err
	}
	if svc.authz, err = p.NewAuthz(svc.pool, svc.provider, svc.queue); err != nil {
		return nil, err
	}
	if svc.telemetry, err = otel.New(p.Service, probe.SpanNames()...); err != nil {
		return nil, err
	}
	if svc.probes, err = probe.Start(svc.telemetry, nil); err != nil {
		return nil, err
	}
	source := authn.KeySource{URL: s.KeysURL, File: s.KeysFile, CAFile: s.CAFile}
	switch {
	case s.Discovery:
		source = authn.KeySource{Issuer: s.Issuer, CAFile: s.CAFile}
	case s.KeyFile != "":
		source = authn.KeySource{PEMFile: s.KeyFile}
	}
	if svc.keys, err = authn.NewKeySet(ctx, source); err != nil {
		return nil, err
	}
	if svc.verifier, err = authn.New(authn.Settings{Issuer: s.Issuer, Audience: s.Audience, Algorithms: s.Algorithms,
		SubjectClaim: s.SubjectClaim, TenantClaim: p.TenantClaim,
		Header: s.Header, RoleClaim: s.RoleClaim, ProblemType: p.ProblemType, ProblemCode: p.ProblemCode,
		Keys: svc.keys}); err != nil {
		return nil, err
	}
	config := command.Config{DB: svc.pool, AuditDB: svc.auditDB, Authz: svc.authz, Audit: svc.writer,
		AuditSettings: p.AuditSettings, Outbox: svc.queue, Probes: svc.probes, Service: p.Service,
		ServiceVersion: s.ServiceVersion, DeclarationVersion: p.DeclarationVersion}
	if len(p.Commands) > 0 {
		config.Entities = p.Commands
		if svc.commands, err = command.New(ctx, config); err != nil {
			return nil, err
		}
	}
	if len(p.Collections) > 0 {
		config.Entities = p.Collections
		if svc.collections, err = command.New(ctx, config); err != nil {
			return nil, err
		}
	}
	if svc.queries, err = query.New(query.Config{DB: svc.pool, AuditDB: svc.auditDB, Audit: svc.writer,
		AuditSettings: p.AuditSettings, Authz: svc.authz, Service: p.Service, CursorKey: s.CursorKey,
		Meter: svc.telemetry.MeterProvider()}); err != nil {
		return nil, err
	}
	return svc, nil
}

// checkSchema refuses a database that does not hold the schema manifest of the service and of the audit record
// (ST-SUP-RP14-LC06): it reads the catalog as the runtime role before New opens anything else, so a refused start
// writes nothing and serves no request. The failure record of the process holds no error text, so it logs the
// faults, which name only objects of the catalog. The audit triggers' functions are those that the audit DDL makes
// for the declared anchor period (Parts.Workers.AnchorPeriod), and audit_maintain is the one of the declared retention
// (Parts.Workers.RetentionMonths).
func checkSchema(ctx context.Context, db *sql.DB, declared manifest.Manifest, owner string, anchorPeriod time.Duration, retentionMonths int) error {
	// The check runs as the runtime role, which alone may run audit_maintain.
	var runtimeRole string
	if err := db.QueryRowContext(ctx, "SELECT current_user").Scan(&runtimeRole); err != nil {
		return fmt.Errorf("service: read the runtime role: %w", err)
	}
	record, err := audit.Manifest(audit.Config{AnchorPeriod: anchorPeriod, RetentionMonths: retentionMonths, RuntimeRole: runtimeRole})
	if err != nil {
		return fmt.Errorf("service: the manifest of the audit record: %w", err)
	}
	want := manifest.Join(declared, record)
	want.Owner = owner
	err = manifest.Check(ctx, db, want)
	var mismatch *manifest.Mismatch
	if errors.As(err, &mismatch) {
		slog.ErrorContext(ctx, "service: the database does not satisfy the schema manifest, and the service does not start",
			"faults", mismatch.Faults)
	}
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return nil
}

// reach checks that SpiceDB answers its health check within ReachTimeout, so a service whose SpiceDB cannot be
// reached does not start.
func reach(ctx context.Context, conn *grpc.ClientConn) error {
	ctx, cancel := context.WithTimeout(ctx, ReachTimeout)
	defer cancel()
	answer, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("service: SpiceDB at the setting %s cannot be reached", SpiceDBEndpointSetting)
	}
	if answer.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("service: SpiceDB at the setting %s is not serving", SpiceDBEndpointSetting)
	}
	return nil
}

// DialSpiceDB connects to SpiceDB with its preshared key, over verified TLS 1.2 or later, or without TLS to a
// loopback address when the settings allow it for a local test.
func DialSpiceDB(s SpiceDB) (*grpc.ClientConn, error) {
	if s.Endpoint == "" || s.Key == "" {
		return nil, fmt.Errorf("service: SpiceDB needs the settings %s and %s", SpiceDBEndpointSetting, SpiceDBKeyFileSetting)
	}
	transport := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if s.InsecureLoopback {
		transport = insecure.NewCredentials()
	}
	return grpc.NewClient(s.Endpoint, grpc.WithTransportCredentials(transport),
		grpc.WithPerRPCCredentials(presharedKey{token: s.Key, plaintext: s.InsecureLoopback}))
}

// presharedKey sends the preshared key of SpiceDB on each call.
type presharedKey struct {
	token     string
	plaintext bool
}

func (k presharedKey) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + k.token}, nil
}

func (k presharedKey) RequireTransportSecurity() bool { return !k.plaintext }

// StartStage gives the stage of a start that New refused, for the failure record of the process, which holds no
// error text (ST-SUP-RP14-LC05): service.start.authn.discovery for a discovery document that the key set refused,
// service.start.authn.keys for another key source that it refused, and empty for every other failure.
func StartStage(err error) string {
	switch {
	case errors.Is(err, authn.ErrDiscovery):
		return "service.start.authn.discovery"
	case errors.Is(err, authn.ErrKeys):
		return "service.start.authn.keys"
	}
	return ""
}

// Verifier is the verifier of the bearer tokens; Middleware verifies the token of each request before next.
func (svc *Service) Verifier() *authn.Verifier { return svc.verifier }

// Middleware verifies the bearer token of each request before next; a request whose token the verifier refuses is
// answered 401 and reaches no handler.
func (svc *Service) Middleware(next http.Handler) http.Handler { return svc.verifier.Middleware(next) }

// Commands is the command runtime of the declared commands, and nil when the service declares none.
func (svc *Service) Commands() *command.Runtime { return svc.commands }

// Collections is the command runtime of the generated collection commands, and nil when the service has none.
func (svc *Service) Collections() *command.Runtime { return svc.collections }

// Queries is the query runtime.
func (svc *Service) Queries() *query.Runtime { return svc.queries }

// Probes is the probe runtime, which binds the probe call of each query call.
func (svc *Service) Probes() *probe.Runtime { return svc.probes }

// Authz is the authorization runtime, Writer the audit writer and Queue the outbox queue.
func (svc *Service) Authz() *authz.Runtime    { return svc.authz }
func (svc *Service) Writer() *audit.Writer    { return svc.writer }
func (svc *Service) Queue() *outbox.Queue     { return svc.queue }
func (svc *Service) Telemetry() *otel.Runtime { return svc.telemetry }

// Close releases what New opened.
func (svc *Service) Close() error {
	var errs []error
	if svc.probes != nil {
		errs = append(errs, svc.probes.Close(context.Background()))
	} else if svc.telemetry != nil {
		errs = append(errs, svc.telemetry.Shutdown(context.Background()))
	}
	if svc.verifier != nil {
		svc.verifier.Stop()
	}
	if svc.keys != nil {
		svc.keys.Stop()
	}
	if svc.spice != nil {
		errs = append(errs, svc.spice.Close())
	}
	for _, db := range []*sql.DB{svc.auditDB, svc.pool} {
		if db != nil {
			errs = append(errs, db.Close())
		}
	}
	return errors.Join(errs...)
}
