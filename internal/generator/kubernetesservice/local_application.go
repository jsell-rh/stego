package kubernetesservice

import (
	"github.com/jsell-rh/stego/internal/browserapplication"
	"strconv"
)

func addLocalApplication(pod object, c *browserapplication.Config) {
	// Only the browser container receives OAuth, session, database, and public
	// TLS material. The application has separate mounts and no service account.
	application := object{
		"name": "application", "image": c.Image, "imagePullPolicy": "IfNotPresent",
		"env":             []any{object{"name": c.ListenEnv, "value": "127.0.0.1"}, object{"name": c.PortEnv, "value": strconv.Itoa(c.Port)}, object{"name": "GOMEMLIMIT", "value": "384MiB"}, object{"name": "GOMAXPROCS", "value": "1"}},
		"envFrom":         []any{object{"secretRef": object{"name": c.EnvSecret}}},
		"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}}},
		"resources":       object{"requests": object{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "32Mi"}, "limits": object{"cpu": "1", "memory": "512Mi", "ephemeral-storage": "128Mi"}},
		"volumeMounts":    []any{object{"name": "application-files", "mountPath": "/var/run/stego-application", "readOnly": true}, object{"name": "application-tmp", "mountPath": "/tmp"}},
	}
	pod["containers"] = append(pod["containers"].([]any), application)
	pod["volumes"] = append(pod["volumes"].([]any), object{"name": "application-files", "secret": object{"secretName": c.FilesSecret, "defaultMode": 288}}, object{"name": "application-tmp", "emptyDir": object{"sizeLimit": "64Mi"}})
}
