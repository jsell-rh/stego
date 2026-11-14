package service

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
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
	// SchemaSteps are the IDs of the schema changes of this release whose last step is expand and that have a sync
	// trigger; the command runtime sets the writer marker to them in each transaction that writes
	// (ST-SUP-RC01-OC23).
	SchemaSteps []string
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
	// WorkerRole and OutboxRelayRole are the roles of the outbox worker and of the outbox relay functions, which the
	// manifest of the outbox names with the runtime role, the role that New connects as (ST-SUP-RP14-LC06).
	WorkerRole, OutboxRelayRole string
	// RelayRole is the login role of the authorization relay, and AuthzSchema the schema of the revocation tables,
	// which the manifest of the authorization runtime names (ST-SUP-RP14-LC06).
	RelayRole, AuthzSchema string
	// PurgeRole is the role that owns the purge function of the key table, which the manifest of the key table names
	// (ST-SUP-RP14-LC06).
	PurgeRole string
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
	case p.WorkerRole == "" || p.OutboxRelayRole == "" || p.RelayRole == "" || p.AuthzSchema == "" || p.PurgeRole == "":
		return errors.New("service: the parts need the worker, outbox relay, relay and purge roles and the authorization schema, which the manifests of the outbox, the authorization runtime and the key table name")
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
	classes, err := checkSchema(ctx, svc.pool, p)
	if err != nil {
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
	// A database that does not satisfy the schema manifest is an alert, not a refusal (F10, owner, 2026-11-13).
	for _, class := range classes {
		svc.probes.SchemaMismatch(ctx, class)
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
	config := command.Config{DB: svc.pool, AuditDB: svc.auditDB, Authz: svc.authz, Audit: svc.writer, SchemaSteps: p.SchemaSteps,
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

// MaxLoggedFaults is the most faults of the schema manifest check that the start of a service logs, each on its own
// line.
const MaxLoggedFaults = 50

// checkSchema checks the database against the schema manifest of the service, of the audit record, of the outbox, of
// the authorization runtime and of the key table (ST-SUP-RP14-LC06): it reads the catalog as the runtime role before
// New opens anything else. A database that does not satisfy the manifest is an alert and not a refusal (decision on
// finding F10 of the schema-change research, owner, 2026-11-13): checkSchema logs each fault, up to MaxLoggedFaults, on a line of its own at
// the level error, with its class (manifest.FaultClass), which names only objects of the catalog and no row value,
// and gives the classes of the faults, each once, so that New emits the probe observation schema.manifest.mismatch
// for each class and starts; the migration job refuses the same database. Any other failure of the check,
// and parts that give no manifest, it refuses. The audit triggers' functions are those that the audit DDL makes for
// the declared anchor period (Parts.Workers.AnchorPeriod), and audit_maintain is the one of the declared retention
// (Parts.Workers.RetentionMonths).
func checkSchema(ctx context.Context, db *sql.DB, p Parts) ([]string, error) {
	// The check runs as the runtime role, which alone may run audit_maintain.
	var runtimeRole string
	if err := db.QueryRowContext(ctx, "SELECT current_user").Scan(&runtimeRole); err != nil {
		return nil, fmt.Errorf("service: read the runtime role: %w", err)
	}
	want, err := joinedManifest(p, runtimeRole)
	if err != nil {
		return nil, err
	}
	err = manifest.Check(ctx, db, want)
	var mismatch *manifest.Mismatch
	if errors.As(err, &mismatch) {
		var classes []string
		for i, fault := range mismatch.Faults {
			class := manifest.FaultClass(fault)
			// At most MaxLoggedFaults lines, and one line with the count of the rest, as a role that can make
			// catalog objects can make thousands of faults (review of PR #305, L2). Each fault still gives its class.
			if i < MaxLoggedFaults {
				slog.ErrorContext(ctx, "service: the database does not satisfy the schema manifest; the service starts, and the migration job refuses it",
					"class", class, "fault", fault)
			}
			if !slices.Contains(classes, class) {
				classes = append(classes, class)
			}
		}
		if rest := len(mismatch.Faults) - MaxLoggedFaults; rest > 0 {
			slog.ErrorContext(ctx, "service: the database does not satisfy the schema manifest; more faults are not logged", "faults", rest)
		}
		return classes, nil
	}
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	return nil, nil
}

// joinedManifest is the schema manifest that New checks at the start and the migration job checks at its end: the
// manifest of the declared parts (Parts.Schema) joined with the manifests of the audit record, the outbox, the
// authorization runtime and the key table for the runtime role runtimeRole, owned by the owner role of the parts.
func joinedManifest(p Parts, runtimeRole string) (manifest.Manifest, error) {
	record, err := audit.Manifest(audit.Config{AnchorPeriod: p.Workers.AnchorPeriod, RetentionMonths: p.Workers.RetentionMonths, RuntimeRole: runtimeRole})
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("service: the manifest of the audit record: %w", err)
	}
	queue, err := outbox.Manifest(p.OutboxRelayRole, runtimeRole, p.WorkerRole)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("service: the manifest of the outbox: %w", err)
	}
	// The outbox relay role owns the state hook functions of the authorization runtime too, so its manifest is in
	// the check, or the confined relay role would own functions that the manifest does not name.
	revocations, err := authz.Manifest(p.AuthzSchema, runtimeRole, p.RelayRole, p.OutboxRelayRole, p.WorkerRole)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("service: the manifest of the authorization runtime: %w", err)
	}
	// The migration job makes the key table in idempotency.KeySchema, where Wrap and Purge name it.
	keys, err := idempotency.Manifest(idempotency.KeySchema, p.PurgeRole, runtimeRole)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("service: the manifest of the key table: %w", err)
	}
	want := manifest.Join(p.Schema, record, queue, revocations, keys)
	want.Owner = p.OwnerRole
	return want, nil
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
// service.start.authn.keys for another key source that it refused, service.start.schema_manifest for a database that
// does not hold the schema manifest (ST-SUP-RP14-LC06), and empty for every other failure.
func StartStage(err error) string {
	switch {
	case errors.Is(err, manifest.ErrNotSatisfied):
		return "service.start.schema_manifest"
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
