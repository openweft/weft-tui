package main

// catalogue_listers.go — one ListXxx wrapper per noun the
// catalogue.go entries consume. Each function takes the WeftAgent
// client + returns a flat []map[string]any the table widget
// renders.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	weftv1 "github.com/openweft/weft-proto"
)

func listNetworks(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListNetworks(ctx, &weftv1.ListNetworksRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Networks))
	for _, n := range resp.Networks {
		out = append(out, map[string]any{
			"uuid": n.Uuid, "name": n.Name, "cidr": n.Cidr,
			"type": n.Type, "project_uuid": n.ProjectUuid,
		})
	}
	return out, nil
}

func listSubnets(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListSubnets(ctx, &weftv1.ListSubnetsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Subnets))
	for _, sn := range resp.Subnets {
		out = append(out, map[string]any{
			"uuid": sn.Uuid, "name": sn.Name, "cidr": sn.Cidr,
			"network_uuid": sn.NetworkUuid, "project_uuid": sn.ProjectUuid,
		})
	}
	return out, nil
}

func listVolumes(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListVolumes(ctx, &weftv1.ListVolumesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		out = append(out, map[string]any{
			"uuid": v.Uuid, "name": v.Name, "size_gib": v.SizeGib,
			"format": v.Format, "project_uuid": v.ProjectUuid,
			"attached_to_uuid": v.AttachedToUuid,
		})
	}
	return out, nil
}

func listShares(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListShares(ctx, &weftv1.ListSharesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Shares))
	for _, sh := range resp.Shares {
		out = append(out, map[string]any{
			"uuid": sh.Uuid, "name": sh.Name, "backend": sh.Backend,
			"size_gb": sh.SizeGb, "project_uuid": sh.ProjectUuid,
			"status": sh.Status,
		})
	}
	return out, nil
}

// listCollections lists iRODS collections (directory-equivalents in
// the iRODS data grid). The weft-ha-irods plugin is the storage
// backend ; it exposes a REST/NATS surface the agent will eventually
// proxy via a weftv1.ListCollections RPC. Until that RPC lands the
// lister returns no rows — the catalogue entry stays visible in the
// Storage section so operators can discover it ; the empty table
// signals "iRODS plugin not wired yet" without erroring out.
func listCollections(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	_ = ctx
	_ = c
	return nil, nil
}

func listBuckets(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListBuckets(ctx, &weftv1.ListBucketsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Buckets))
	for _, b := range resp.Buckets {
		out = append(out, map[string]any{
			"uuid": b.Uuid, "name": b.Name, "endpoint": b.Endpoint,
			"region": b.Region, "project_uuid": b.ProjectUuid,
		})
	}
	return out, nil
}

func listFloatingIPs(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListFloatingIPs(ctx, &weftv1.ListFloatingIPsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.FloatingIps))
	for _, f := range resp.FloatingIps {
		out = append(out, map[string]any{
			"uuid": f.Uuid, "address": f.Address,
			"network_uuid":   f.Network,
			"mapped_to_uuid": f.MappedTo,
			"project_uuid":   f.ProjectUuid,
		})
	}
	return out, nil
}

func listLoadBalancers(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListLoadBalancers(ctx, &weftv1.ListLoadBalancersRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.LoadBalancers))
	for _, lb := range resp.LoadBalancers {
		out = append(out, map[string]any{
			"uuid": lb.Uuid, "name": lb.Name,
			"frontend":       lb.ListenAddr,
			"backends_count": len(lb.Backends),
			"project_uuid":   lb.ProjectUuid,
		})
	}
	return out, nil
}

func listDNSZones(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListDNSZones(ctx, &weftv1.ListDNSZonesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Zones))
	for _, z := range resp.Zones {
		out = append(out, map[string]any{
			"uuid": z.Uuid, "name": z.Name,
			"default_ttl":  z.Ttl,
			"project_uuid": z.ProjectUuid,
		})
	}
	return out, nil
}

func listDNSRecords(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	// ListDNSRecordsRequest.zone_uuid is required server-side ; the
	// catalogue view wants every record across every zone, so we
	// fan out : list zones first, then per-zone List, then concat.
	// Per-zone failures are reported in-row (so a single broken
	// zone doesn't blank the whole view) rather than aborted.
	zones, err := c.ListDNSZones(ctx, &weftv1.ListDNSZonesRequest{})
	if err != nil {
		return nil, fmt.Errorf("list dns zones: %w", err)
	}
	out := make([]map[string]any, 0, 16)
	for _, z := range zones.Zones {
		resp, err := c.ListDNSRecords(ctx, &weftv1.ListDNSRecordsRequest{ZoneUuid: z.Uuid})
		if err != nil {
			out = append(out, map[string]any{
				"uuid": "", "name": "<error>", "type": "",
				"value": err.Error(), "zone_uuid": z.Uuid,
			})
			continue
		}
		for _, r := range resp.Records {
			out = append(out, map[string]any{
				"uuid": r.Uuid, "name": r.Name, "type": r.Type,
				"value": r.Value, "zone_uuid": r.ZoneUuid,
			})
		}
	}
	return out, nil
}

func listSecurityGroups(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListSecurityGroups(ctx, &weftv1.ListSecurityGroupsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Groups))
	for _, g := range resp.Groups {
		out = append(out, map[string]any{
			"uuid": g.Uuid, "name": g.Name,
			"description":  g.Description,
			"project_uuid": g.ProjectUuid,
		})
	}
	return out, nil
}

func listSchedulingRules(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListSchedulingRules(ctx, &weftv1.ListSchedulingRulesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Rules))
	for _, r := range resp.Rules {
		out = append(out, map[string]any{
			"uuid": r.Uuid, "name": r.Name,
			"selector":     r.Selector,
			"target_count": r.TargetCount,
		})
	}
	return out, nil
}

func listTenants(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListTenants(ctx, &weftv1.ListTenantsRequest{})
	if err != nil {
		return nil, err
	}
	// Side-load projects + VMs to compute the per-tenant VM total
	// (sum across the tenant's projects). 2026-06-24 operator
	// directive : "dans la vue tenant ca serait bien d'avoir une
	// colonne avec la sommes des VMS de tous les projets du
	// tenant". Best-effort : on either side-load error, the column
	// stays blank (0) — the tenant list still renders.
	vmsByProject := map[string]int{}
	if vmResp, vErr := c.ListVMs(ctx, &weftv1.ListVMsRequest{}); vErr == nil {
		for _, v := range vmResp.Vms {
			if v.ProjectUuid != "" {
				vmsByProject[v.ProjectUuid]++
			}
		}
	}
	projectsByTenant := map[string]int{}
	if pResp, pErr := c.ListProjects(ctx, &weftv1.ListProjectsRequest{}); pErr == nil {
		for _, p := range pResp.Projects {
			projectsByTenant[p.TenantUuid] += vmsByProject[p.Uuid]
		}
	}
	out := make([]map[string]any, 0, len(resp.Tenants))
	for _, t := range resp.Tenants {
		out = append(out, map[string]any{
			"uuid": t.Uuid, "name": t.Name, "domain": t.Domain,
			"status":        t.Status,
			"admins_count":  t.Admins,
			"members_count": t.Members,
			"vms_count":     projectsByTenant[t.Uuid],
		})
	}
	return out, nil
}

func listUsers(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListUsers(ctx, &weftv1.ListUsersRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Users))
	for _, u := range resp.Users {
		out = append(out, map[string]any{
			"uuid":         u.Uuid,
			"email":        u.Email,
			"display_name": u.DisplayName,
			"oidc_issuer":  u.OidcIssuer,
		})
	}
	return out, nil
}

func listSSHKeyCatalogue(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListSSHKeyCatalogue(ctx, &weftv1.ListSSHKeyCatalogueRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Keys))
	for _, k := range resp.Keys {
		out = append(out, map[string]any{
			"name":        k.Name,
			"fingerprint": k.Fingerprint,
			"source":      k.Comment,
		})
	}
	return out, nil
}

// flavorShape is the (vcpu, ram_gib) tuple synthFlavors dedupes
// against.
type flavorShape struct {
	vcpu int
	gib  int
}

// ramToGiB parses a Flavor.ram string ("4Gi", "256Mi", raw number
// = MB) into integer GiB. Returns 0 on parse failure — the caller
// then treats the shape as "no RAM specified" for matching.
func ramToGiB(s string) int {
	if s == "" {
		return 0
	}
	// "Xgi" / "Xgib" / "XG"
	low := strings.ToLower(s)
	switch {
	case strings.HasSuffix(low, "gi") || strings.HasSuffix(low, "gib") || strings.HasSuffix(low, "g"):
		n := strings.TrimRight(low, "gib")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v
	case strings.HasSuffix(low, "mi") || strings.HasSuffix(low, "mib") || strings.HasSuffix(low, "m"):
		n := strings.TrimRight(low, "mib")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v / 1024
	}
	// Bare digits = MB.
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v / 1024
}

// ramToMB is the same parser as ramToGiB but emits an MB-precision
// result so sub-GiB shapes (256Mi, 512Mi — small infra VMs) match
// the cache key. Bare digits and Xg/Gi units are converted to MB ;
// Mi/Mib/m stays in its native MB unit. Returns 0 on parse failure.
func ramToMB(s string) int {
	if s == "" {
		return 0
	}
	low := strings.ToLower(s)
	switch {
	case strings.HasSuffix(low, "gi") || strings.HasSuffix(low, "gib") || strings.HasSuffix(low, "g"):
		n := strings.TrimRight(low, "gib")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v * 1024
	case strings.HasSuffix(low, "mi") || strings.HasSuffix(low, "mib") || strings.HasSuffix(low, "m"):
		n := strings.TrimRight(low, "mib")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

// synthFlavors derives "in-use" flavor entries from the running
// VM set : groups VMs by (vcpu, ram_gib), counts how many VMs share
// the shape, and emits one synthetic row per shape. Surfaces the
// implicit catalogue even when the curated flavors registry is
// empty — operator directive 2026-06-24 "si on a des VM qui
// tournent, il y'a forcement des flavors". Best-effort : on
// ListVMs error returns nil so listFlavors still renders the
// registered shapes.
func synthFlavors(ctx context.Context, c weftv1.WeftAgentClient, seenNames map[string]bool) []map[string]any {
	vmResp, err := c.ListVMs(ctx, &weftv1.ListVMsRequest{})
	if err != nil {
		return nil
	}
	counts := map[flavorShape]int{}
	for _, v := range vmResp.Vms {
		gib := int(v.MemMb) / 1024
		if gib == 0 && v.MemMb > 0 {
			gib = 1
		}
		counts[flavorShape{vcpu: int(v.Cpu), gib: gib}]++
	}
	out := make([]map[string]any, 0, len(counts))
	for shape, n := range counts {
		if shape.vcpu == 0 && shape.gib == 0 {
			continue
		}
		name := fmt.Sprintf("auto:%dcpu-%dgib", shape.vcpu, shape.gib)
		if seenNames[name] {
			continue
		}
		out = append(out, map[string]any{
			"name":      name,
			"vcpu":      uint32(shape.vcpu),
			"ram_gib":   uint64(shape.gib),
			"gpu":       "", // no GPU info on auto-derived shapes
			"vm_count":  n,
			"synthetic": true,
		})
	}
	return out
}

func listFlavors(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListFlavors(ctx, &weftv1.ListFlavorsRequest{})
	if err != nil {
		return nil, err
	}
	// vmsByShape counts how many VMs match each (vcpu, ram_gib)
	// tuple so the VMS column on registered flavors gets populated
	// too — not just the auto-derived synthetic entries.
	vmsByShape := map[flavorShape]int{}
	if vmResp, vErr := c.ListVMs(ctx, &weftv1.ListVMsRequest{}); vErr == nil {
		for _, v := range vmResp.Vms {
			gib := int(v.MemMb) / 1024
			if gib == 0 && v.MemMb > 0 {
				gib = 1
			}
			vmsByShape[flavorShape{vcpu: int(v.Cpu), gib: gib}]++
		}
	}
	out := make([]map[string]any, 0, len(resp.Flavors))
	seen := map[string]bool{}
	for _, f := range resp.Flavors {
		seen[f.Name] = true
		gib := int(0)
		// Best-effort RAM parsing : "Xgi" or raw number = MB.
		if v := f.Ram; v != "" {
			gib = ramToGiB(v)
		}
		out = append(out, map[string]any{
			"uuid":     f.Uuid,
			"name":     f.Name,
			"vcpu":     f.Vcpu,
			"ram_gib":  f.Ram,
			"gpu":      f.Gpu,
			"vm_count": vmsByShape[flavorShape{vcpu: int(f.Vcpu), gib: gib}],
		})
	}
	// Append synthetic "in-use" shapes derived from running VMs so
	// the panel never reads as empty when the cluster has VMs.
	out = append(out, synthFlavors(ctx, c, seen)...)
	return out, nil
}

func listAZs(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListAZs(ctx, &weftv1.ListAZsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Azs))
	for _, a := range resp.Azs {
		out = append(out, map[string]any{
			"uuid": a.Uuid, "code": a.Code, "name": a.Name,
			"status": a.Status,
		})
	}
	return out, nil
}

func listRacks(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListRacks(ctx, &weftv1.ListRacksRequest{})
	if err != nil {
		return nil, err
	}
	// Side-load the AZ catalogue to resolve uuid → code so the rack
	// table shows the human-friendly AZ name alongside the UUID
	// (operator directive 2026-06-24 : "j'aimerais avoir les deux,
	// l'uuid est déterministe mais le nom est plus humain").
	// Best-effort : on ListAZs error we just leave az_code empty,
	// the column falls through to the raw uuid.
	azCodeByUUID := map[string]string{}
	azNameByUUID := map[string]string{}
	if azResp, azErr := c.ListAZs(ctx, &weftv1.ListAZsRequest{}); azErr == nil {
		for _, a := range azResp.Azs {
			azCodeByUUID[a.Uuid] = a.Code
			azNameByUUID[a.Uuid] = a.Name
		}
	}
	out := make([]map[string]any, 0, len(resp.Racks))
	for _, r := range resp.Racks {
		out = append(out, map[string]any{
			"uuid":     r.Uuid,
			"code":     r.Code,
			"status":   r.Status,
			"az_code":  azCodeByUUID[r.AzUuid],
			"az_name":  azNameByUUID[r.AzUuid],
			"az_uuid":  r.AzUuid,
			"position": r.Hosts, // re-uses the position column slot to show host count
		})
	}
	return out, nil
}

// knownInfraImages is the seed list of OCI references the cluster
// pulls on bring-up : the microVM kernel + pod-initrd plus the core
// infra services (etcd / NATS / DNS / OIDC / OCI registry / web UI).
// Sourced from [project_infra_images_forked] + [project_weft_up_gaps]
// + the agent's zombiegc audit on the live cluster. Tag pinning is
// kept light — the ":latest" suffix is what the catalogue actually
// pulls today ; pinned tags will replace these when the supply-chain
// story tightens.
//
// listImages overlays this set onto the agent's local OCI cache so
// operators see both "cached" (already pulled, ready to boot a VM)
// and "available" (seedable from GHCR via the `p` pull action). Same
// merging pattern as the Plugins view's catalogue + installed merge.
var knownInfraImages = []struct {
	URL  string
	Role string // human-readable hint shown in the NAME column when uncached
}{
	{"ghcr.io/openweft/weft-microvm-kernel:latest", "microvm-kernel"},
	{"ghcr.io/openweft/weft-microvm-pod-initrd:latest", "microvm-pod-initrd"},
	{"ghcr.io/openweft/weft-etcd:latest", "infra-etcd"},
	{"ghcr.io/openweft/weft-nats:latest", "infra-nats"},
	{"ghcr.io/openweft/weft-dex:latest", "infra-dex"},
	{"ghcr.io/openweft/weft-zot:latest", "infra-zot"},
	{"ghcr.io/openweft/weft-coredns:latest", "infra-coredns"},
	{"ghcr.io/openweft/weft-webui:latest", "infra-webui"},
}

func listImages(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListImages(ctx, &weftv1.ListImagesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Images)+len(knownInfraImages))
	cached := map[string]bool{}
	for _, img := range resp.Images {
		out = append(out, map[string]any{
			"url": img.Url, "name": img.Name,
			"format":     img.Format,
			"size_bytes": img.SizeBytes,
			"state":      "cached",
		})
		cached[img.Url] = true
	}
	// Append the infra images that aren't yet in the local cache so
	// operators can pull them on demand from the same view.
	for _, ki := range knownInfraImages {
		if cached[ki.URL] {
			continue
		}
		out = append(out, map[string]any{
			"url": ki.URL, "name": ki.Role,
			"format":     "oci",
			"size_bytes": int64(0),
			"state":      "available",
		})
	}
	return out, nil
}

// installedPluginsMsg is the response payload of loadInstalledPluginsCmd.
// names is the set of plugin catalogue names that have at least one
// instance with a non-empty status — that's the gate the sidebar
// RequiresPlugin filter consults. err non-nil means we don't know yet
// and the Model leaves installedPlugins unchanged (the sidebar stays
// permissive until the next tick clarifies).
type installedPluginsMsg struct {
	names map[string]bool
	err   error
}

// loadInstalledPluginsCmd runs ListInstalledPlugins, returns the set
// of catalogue names that have running instances. Used by the
// sidebar gate on RequiresPlugin ; re-armed by refreshTickMsg so a
// freshly-installed plugin lights up its sidebar entries within one
// refresh interval. Takes the narrow PluginsClient so tests can
// inject a fake without satisfying the full WeftAgent surface.
func loadInstalledPluginsCmd(client PluginsClient) tea.Cmd {
	if client == nil {
		return func() tea.Msg {
			return installedPluginsMsg{err: errNoClient}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := client.ListInstalledPlugins(ctx, &weftv1.ListInstalledPluginsRequest{})
		if err != nil {
			return installedPluginsMsg{err: err}
		}
		// Sidebar gate ignores disabled instances : an operator who
		// pressed `x` to hide a plugin's items expects them gone from
		// the sidebar until they press `e` again. The Plugins view
		// itself still shows the row (state="disabled") so re-enable
		// stays one keystroke away.
		names := make(map[string]bool, len(resp.Instances))
		for _, p := range resp.Instances {
			if p.Name != "" && !p.Disabled {
				names[p.Name] = true
			}
		}
		return installedPluginsMsg{names: names}
	}
}

// staticPluginCatalogue mirrors the openweft/.../catalogue/ HCL tree
// so the TUI always has something to show even when the agent's live
// catalogue is empty or unreachable. Same pattern weft-webui uses
// ([feedback_static_catalogue_fallback]) : the live catalogue wins
// when populated, the static list fills the gap otherwise. Keeps
// names byte-identical with the `plugin "<name>" {}` block on each
// HCL — that's the catalogue identity install + the sidebar
// RequiresPlugin gate consult.
var staticPluginCatalogue = []struct {
	Name, Version, Kind, Description string
}{
	{"caddy-edge", "v1", "edge-proxy", "Caddy edge proxy / load balancer"},
	{"forgejo-ha", "v1", "git-forge", "Forgejo git forge, 3-replica HA"},
	{"forgejo-runners-ha", "v1", "ci-runners", "Forgejo Actions runner farm"},
	{"github-runners-ha", "v1", "ci-runners", "Self-hosted GitHub Actions runner farm"},
	{"gitlab-runners-ha", "v1", "ci-runners", "Self-hosted GitLab runner farm"},
	{"grafana-ha", "v1", "observability", "Grafana dashboards, 3-replica HA"},
	{"irods-ha", "v1", "data-management", "iRODS HA — 3 catalog providers + shared Postgres iCAT"},
	{"jupyterhub-ha", "v1", "compute", "JupyterHub multi-user notebook portal"},
	{"loki-ha", "v1", "observability", "Loki log aggregation, 3-replica HA"},
	{"loom-ha", "v1", "collaborative-editor", "Three weft-loom-server replicas behind Caddy, SSO via dex"},
	{"postgres-ha", "v1", "database", "PostgreSQL HA — 3-replica streaming replication"},
	{"prometheus-ha", "v1", "observability", "Prometheus metrics, 3-replica HA"},
	{"redis-ha", "v1", "cache", "Redis HA — 3-replica with Sentinel"},
	{"vault-ha", "v1", "secrets", "HashiCorp Vault HA"},
	{"versitygw-ha", "v1", "object-storage", "VersityGW S3 object gateway (CubeFS-backed)"},
}

// listInstalledPlugins merges the catalogue (all available plugins
// the agent knows about) with the currently-installed instances so
// the Plugins view shows BOTH "installed" and "available" rows.
// Operators install from the same view (key `i`), which the user
// expects per the 2026-06-26 directive : "irods est a declarer comme
// plugin dans Installed plugins avec la possibilité de l'installer".
//
// Identity : (catalogue.name, instance.project) tuple — multiple
// projects can each install the same catalogue entry independently.
// Rows whose project is empty represent "available, not yet
// installed in any project".
//
// Fallback : when the live ListPluginCatalogue returns empty or
// errors, the static catalogue above fills in. Operators reported
// "la liste plugins est vide" on the live cluster 2026-06-26 because
// the agent's catalogue root was misconfigured ; same lesson as
// [feedback_static_catalogue_fallback] applied to the webui earlier.
func listInstalledPlugins(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	catResp, catErr := c.ListPluginCatalogue(ctx, &weftv1.ListPluginCatalogueRequest{})
	instResp, instErr := c.ListInstalledPlugins(ctx, &weftv1.ListInstalledPluginsRequest{})
	if instErr != nil {
		return nil, instErr
	}
	// Build a name → kind lookup from BOTH sources so installed rows
	// (whose PluginInstance proto carries no kind) inherit the field
	// from the catalogue entry. Live catalogue wins, static list
	// fills in.
	kinds := map[string]string{}
	if catErr == nil {
		for _, e := range catResp.Entries {
			if e.Kind != "" {
				kinds[e.Name] = e.Kind
			}
		}
	}
	for _, e := range staticPluginCatalogue {
		if _, ok := kinds[e.Name]; !ok {
			kinds[e.Name] = e.Kind
		}
	}

	// installed = name → true when an ENABLED instance exists. The
	// sidebar's RequiresPlugin gate consumes the same set, so a
	// disabled instance should NOT keep the gate open — that's the
	// point of EnablePlugin / DisablePlugin (commit ce47616 follow-up).
	// Disabled rows still appear in the Plugins view (state="disabled")
	// so operators can re-enable them.
	installed := map[string]bool{}
	out := make([]map[string]any, 0)
	for _, p := range instResp.Instances {
		state := p.Status
		if state == "" {
			state = "installed"
		}
		out = append(out, map[string]any{
			"uuid":         p.InstanceUuid,
			"name":         p.Name,
			"version":      "",
			"kind":         kinds[p.Name],
			"state":        state,
			"disabled":     p.Disabled,
			"project_uuid": p.Project, // kept on the row map for the detail drawer
		})
		if !p.Disabled {
			installed[p.Name] = true
		}
	}
	// Live catalogue wins when populated. Empty / errored ones fall
	// through to the static list so the operator always sees what
	// could be installed — same UX principle as the webui's
	// /api/plugins/catalogue (cf. feedback_static_catalogue_fallback).
	//
	// "inputs" is preserved on each available row so the `i` install
	// action's Fields function can produce a per-plugin prompt
	// without a second RPC. Stored as []*weftv1.PluginInput so the
	// downstream type assertion stays explicit ; consumers should
	// nil-check before iterating since the static fallback omits it.
	if catErr == nil && len(catResp.Entries) > 0 {
		for _, e := range catResp.Entries {
			if installed[e.Name] {
				continue
			}
			out = append(out, map[string]any{
				"uuid":         "",
				"name":         e.Name,
				"version":      e.Version,
				"kind":         e.Kind,
				"state":        "available",
				"project_uuid": "",
				"inputs":       e.Inputs,
			})
		}
	} else {
		for _, e := range staticPluginCatalogue {
			if installed[e.Name] {
				continue
			}
			out = append(out, map[string]any{
				"uuid":         "",
				"name":         e.Name,
				"version":      e.Version,
				"kind":         e.Kind,
				"state":        "available",
				"project_uuid": "",
				// staticPluginCatalogue carries no input schema ;
				// the install action handles a nil/empty list by
				// installing with no inputs (agent will reject if
				// the plugin needs any, surfacing the error
				// straight to the operator).
			})
		}
	}
	return out, nil
}

func listVolumeSnapshots(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListVolumeSnapshots(ctx, &weftv1.ListVolumeSnapshotsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Snapshots))
	for _, sn := range resp.Snapshots {
		out = append(out, map[string]any{
			"uuid": sn.Uuid, "name": sn.Name,
			"volume_uuid":  sn.VolumeUuid,
			"size_gib":     sn.SizeGib,
			"project_uuid": sn.Project,
		})
	}
	return out, nil
}

func listVolumeBackups(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListVolumeBackups(ctx, &weftv1.ListVolumeBackupsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Backups))
	for _, b := range resp.Backups {
		out = append(out, map[string]any{
			"uuid": b.Url, // backups are keyed by url, not uuid — alias for the table
			"url":  b.Url,
			"name": b.Url, // no Name field ; show the url in the name column
			"volume_uuid": b.VolumeUuid,
			"size_gib":    b.SizeBytes / (1024 * 1024 * 1024),
			"status":      b.State,
		})
	}
	return out, nil
}
