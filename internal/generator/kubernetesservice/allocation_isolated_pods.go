package kubernetesservice

import (
	"fmt"
	"regexp"
	"strings"
)

var allocationPinnedImage = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

// The runtime is an operator trust boundary. These settings do not install or
// qualify its handler. They permit root inside that runtime, never host access.
func allocationIsolatedPodConfig(values object, p *allocationProfile) error {
	if raw, exists := values["pod_security"]; exists {
		mode, ok := raw.(string)
		if !ok || (mode != "restricted" && mode != "isolated-runtime") {
			return fmt.Errorf("pod_security requires restricted or isolated-runtime")
		}
		if mode == "isolated-runtime" {
			p.PodSecurity = mode
		}
	}
	if p.PodSecurity == "isolated-runtime" && (p.PodRuntimeClass == "" || p.PodServiceAccount == "" || values["network_isolation"] != true) {
		return fmt.Errorf("isolated-runtime requires pod_runtime_class, pod_service_account, and network_isolation")
	}
	if raw, exists := values["pod_annotations"]; exists {
		entries, ok := raw.([]any)
		if p.PodSecurity != "isolated-runtime" || !ok || len(entries) == 0 || len(entries) > 32 {
			return fmt.Errorf("pod_annotations requires isolated-runtime and 1..32 keys")
		}
		seen := map[string]bool{}
		for _, raw := range entries {
			key, ok := raw.(string)
			domain, _, qualified := strings.Cut(key, "/")
			if !ok || !qualified || !validLabelKey(key) || seen[key] || domain == "kubernetes.io" || strings.HasSuffix(domain, ".kubernetes.io") || domain == "openshift.io" || strings.HasSuffix(domain, ".openshift.io") || strings.Contains(domain, "katacontainers") {
				return fmt.Errorf("Pod annotation requires a distinct application key")
			}
			seen[key] = true
			p.PodAnnotations = append(p.PodAnnotations, key)
		}
	}
	raw, exists := values["pod_capability_grants"]
	if !exists {
		return nil
	}
	entries, ok := raw.([]any)
	if p.PodSecurity != "isolated-runtime" || !ok || len(entries) == 0 || len(entries) > 8 {
		return fmt.Errorf("pod_capability_grants requires isolated-runtime and 1..8 grants")
	}
	// Exact Linux capability names reject spelling errors and wildcard grants.
	known := " CHOWN DAC_OVERRIDE DAC_READ_SEARCH FOWNER FSETID KILL SETGID SETUID SETPCAP LINUX_IMMUTABLE NET_BIND_SERVICE NET_BROADCAST NET_ADMIN NET_RAW IPC_LOCK IPC_OWNER SYS_MODULE SYS_RAWIO SYS_CHROOT SYS_PTRACE SYS_PACCT SYS_ADMIN SYS_BOOT SYS_NICE SYS_RESOURCE SYS_TIME SYS_TTY_CONFIG MKNOD LEASE AUDIT_WRITE AUDIT_CONTROL SETFCAP MAC_OVERRIDE MAC_ADMIN SYSLOG WAKE_ALARM BLOCK_SUSPEND AUDIT_READ PERFMON BPF CHECKPOINT_RESTORE "
	names := map[string]bool{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || len(entry) != 3 {
			return fmt.Errorf("Pod capability grant requires name, image, and capabilities")
		}
		name, nok := entry["name"].(string)
		image, iok := entry["image"].(string)
		caps, cok := entry["capabilities"].([]any)
		if !nok || !label.MatchString(name) || names[name] || !iok || len(image) > 512 || !allocationPinnedImage.MatchString(image) || !cok || len(caps) == 0 || len(caps) > 16 {
			return fmt.Errorf("invalid Pod capability grant")
		}
		names[name] = true
		grant := allocationCapabilityGrant{Name: name, Image: image}
		seen := map[string]bool{}
		for _, raw := range caps {
			cap, ok := raw.(string)
			if !ok || cap == "" || strings.ContainsAny(cap, " \t\r\n") || !strings.Contains(known, " "+cap+" ") || seen[cap] {
				return fmt.Errorf("Pod capabilities require distinct Linux capability names")
			}
			seen[cap] = true
			grant.Capabilities = append(grant.Capabilities, cap)
		}
		p.PodCapabilityGrants = append(p.PodCapabilityGrants, grant)
	}
	return nil
}

func allocationPodSecurityLevel(p allocationProfile) string {
	if p.PodSecurity == "isolated-runtime" {
		return "privileged"
	}
	return "restricted"
}

func allocationIsolatedPodChecks(p allocationProfile, checks []any) ([]any, []any) {
	variables := []any{object{"name": "containers", "expression": "object.spec.containers + (has(object.spec.initContainers) ? object.spec.initContainers : [])"}}
	add := func(expression, message string) {
		checks = append(checks, object{"expression": expression, "message": message})
	}
	add("(!has(object.spec.os) || object.spec.os.name == 'linux') && (!has(object.spec.hostNetwork) || !object.spec.hostNetwork) && (!has(object.spec.hostPID) || !object.spec.hostPID) && (!has(object.spec.hostIPC) || !object.spec.hostIPC)", "Isolated Pods require Linux and cannot use host namespaces")
	add("!has(object.spec.ephemeralContainers) || size(object.spec.ephemeralContainers) == 0", "Isolated Pods cannot add ephemeral containers")
	add("!has(object.spec.resourceClaims) || size(object.spec.resourceClaims) == 0", "Isolated Pods cannot request dynamic devices")
	add("!has(object.spec.securityContext) || ((!has(object.spec.securityContext.sysctls) || size(object.spec.securityContext.sysctls) == 0) && !has(object.spec.securityContext.windowsOptions))", "Isolated Pods cannot set sysctls or Windows options")
	add("variables.containers.all(c, !has(c.securityContext) || ((!has(c.securityContext.privileged) || !c.securityContext.privileged) && (!has(c.securityContext.allowPrivilegeEscalation) || !c.securityContext.allowPrivilegeEscalation) && (!has(c.securityContext.procMount) || c.securityContext.procMount == 'Default') && !has(c.securityContext.windowsOptions)))", "Isolated containers cannot request privileged mode, privilege escalation, or unmasked proc storage")
	add("variables.containers.all(c, !has(c.ports) || c.ports.all(p, !has(p.hostPort) || p.hostPort == 0))", "Isolated containers cannot bind host ports")
	add("variables.containers.all(c, (!has(c.volumeDevices) || size(c.volumeDevices) == 0) && (!has(c.volumeMounts) || c.volumeMounts.all(m, !has(m.mountPropagation) || m.mountPropagation == 'None')))", "Isolated containers cannot mount block devices or propagate mounts")
	add("variables.containers.all(c, !has(c.resources) || ((!has(c.resources.claims) || size(c.resources.claims) == 0) && (!has(c.resources.limits) || c.resources.limits.all(k, k in ['cpu','memory','ephemeral-storage'])) && (!has(c.resources.requests) || c.resources.requests.all(k, k in ['cpu','memory','ephemeral-storage']))))", "Isolated containers cannot request extended resources or devices")
	add("!has(object.spec.volumes) || object.spec.volumes.all(v, has(v.configMap) || has(v.downwardAPI) || has(v.emptyDir) || has(v.persistentVolumeClaim) || has(v.projected) || has(v.secret) || has(v.image))", "Isolated Pods require approved volume types")
	annotations := []string{celString("openshift.io/scc"), celString("security.openshift.io/validated-scc-subject-type")}
	for _, key := range p.PodAnnotations {
		annotations = append(annotations, celString(key))
	}
	add("!has(object.metadata.annotations) || object.metadata.annotations.all(k, k in ["+strings.Join(annotations, ",")+"])", "Isolated Pods cannot set undeclared annotations")
	choices := []string{}
	for _, grant := range p.PodCapabilityGrants {
		caps := []string{}
		for _, cap := range grant.Capabilities {
			caps = append(caps, celString(cap))
		}
		choices = append(choices, "(c.name == "+celString(grant.Name)+" && c.image == "+celString(grant.Image)+" && c.securityContext.capabilities.add.all(a, a in ["+strings.Join(caps, ",")+"]))")
	}
	permission := "false"
	if len(choices) > 0 {
		permission = "(" + strings.Join(choices, " || ") + ")"
	}
	add("variables.containers.all(c, !has(c.securityContext) || !has(c.securityContext.capabilities) || !has(c.securityContext.capabilities.add) || size(c.securityContext.capabilities.add) == 0 || (has(c.securityContext.allowPrivilegeEscalation) && !c.securityContext.allowPrivilegeEscalation && has(c.securityContext.capabilities.drop) && 'ALL' in c.securityContext.capabilities.drop && "+permission+"))", "Added capabilities require a declared container name, pinned image, and capability set")
	return variables, checks
}
