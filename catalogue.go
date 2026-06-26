package main

// catalogue.go — the 21 ResourceConfigs the command palette serves.
// One entry per noun ; reusable across every operator pad (CLI ↔
// webui parity). Adding a new noun = adding one entry here.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/charmbracelet/bubbles/table"
	weftv1 "github.com/openweft/weft-proto"
)

// resourceCatalogue is the registry consulted by the command
// palette. ID matches the slug the user types after `:` (e.g.
// `:networks`, `:volumes`).
var resourceCatalogue = []ResourceConfig{
	{
		ID: "networks", Title: "Networks", Section: "Network",
		Columns: []table.Column{
			{Title: "NAME", Width: 20}, {Title: "CIDR", Width: 18},
			{Title: "TYPE", Width: 10}, {Title: "PROJECT", Width: 18},
		},
		List:       listNetworks,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "cidr"), s(r, "type"), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteNetwork(ctx, &weftv1.DeleteNetworkRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "project", Label: "Project (name or UUID)", Required: true},
			{Key: "name", Label: "Name", Required: true},
			{Key: "cidr", Label: "CIDR", Placeholder: "10.42.0.0/24", Required: true},
			{Key: "gateway", Label: "Gateway (optional)", Placeholder: "10.42.0.1"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.CreateNetwork(ctx, &weftv1.CreateNetworkRequest{
				Project: v["project"], Name: v["name"], Cidr: v["cidr"], Gateway: v["gateway"],
			})
			if err != nil {
				return "", err
			}
			return "created network " + v["name"], nil
		},
	},
	{
		ID: "subnets", Title: "Subnets", Section: "Network",
		Columns: []table.Column{
			{Title: "NAME", Width: 16},
			{Title: "CIDR", Width: 18},
			{Title: "NETWORK_UUID", Width: 36},
			{Title: "PROJECT_UUID", Width: 36},
		},
		List:       listSubnets,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "cidr"), s(r, "network_uuid"), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteSubnet(ctx, &weftv1.DeleteSubnetRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "network_uuid", Label: "Network UUID", Required: true},
			{Key: "name", Label: "Name", Required: true},
			{Key: "cidr", Label: "CIDR", Placeholder: "10.42.1.0/24", Required: true},
			{Key: "gateway", Label: "Gateway (optional)", Placeholder: "10.42.1.1"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.CreateSubnet(ctx, &weftv1.CreateSubnetRequest{
				NetworkUuid: v["network_uuid"], Name: v["name"],
				Cidr: v["cidr"], Gateway: v["gateway"],
			})
			if err != nil {
				return "", err
			}
			return "created subnet " + v["name"], nil
		},
		EditFields: []FormField{
			{Key: "name", Label: "Name (empty=keep current)"},
			{Key: "description", Label: "Description (empty=keep current)"},
			{Key: "gateway", Label: "Gateway (empty=keep current)"},
		},
		EditFn: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any, v map[string]string) (string, error) {
			uuid := s(row, "uuid")
			_, err := c.UpdateSubnet(ctx, &weftv1.UpdateSubnetRequest{
				Uuid: uuid, Name: v["name"],
				Description: v["description"], Gateway: v["gateway"],
			})
			if err != nil {
				return "", err
			}
			return "updated subnet " + s(row, "name"), nil
		},
	},
	{
		ID: "volumes", Title: "Volumes", Section: "Storage",
		// Block-volume backend in openweft = weft-block (fork-and-
		// adapt of longhorn-engine, cf. [project_weft_block]). Gate
		// the sidebar entry on its catalogue plugin so operators
		// see Volumes only after the storage backend is deployed —
		// same UX as Collections / Shares / Buckets.
		RequiresPlugin: "weft-block",
		Columns: []table.Column{
			{Title: "NAME", Width: 20}, {Title: "SIZE-GIB", Width: 10},
			{Title: "FORMAT", Width: 10}, {Title: "PROJECT", Width: 18}, {Title: "ATTACHED", Width: 18},
		},
		List:       listVolumes,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), iStr(r["size_gib"]), s(r, "format"), s(r, "project_uuid"), s(r, "attached_to_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteVolume(ctx, &weftv1.DeleteVolumeRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "project", Label: "Project", Required: true},
			{Key: "name", Label: "Name", Required: true},
			{Key: "size_gib", Label: "Size (GiB)", Required: true, Numeric: true},
			{Key: "format", Label: "Format", Placeholder: "raw | qcow2"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			size, _ := strconv.Atoi(v["size_gib"])
			_, err := c.CreateVolume(ctx, &weftv1.CreateVolumeRequest{
				Project: v["project"], Name: v["name"],
				SizeGib: int64(size), Format: v["format"],
			})
			if err != nil {
				return "", err
			}
			return "created volume " + v["name"], nil
		},
	},
	{
		ID: "shares", Title: "Shares", Section: "Storage",
		// Shared filesystem volumes are served by the CubeFS plugin
		// (catalogue id "cubefs") in openweft's reference stack ;
		// other backends would re-claim this entry by name once they
		// register their own catalogue plugin. Sidebar gate hides
		// the entry until cubefs is installed — see [feedback_no_minio]
		// for the openweft storage-backend policy.
		RequiresPlugin: "cubefs",
		Columns: []table.Column{
			{Title: "NAME", Width: 20}, {Title: "BACKEND", Width: 10},
			{Title: "SIZE-GB", Width: 10}, {Title: "PROJECT", Width: 18}, {Title: "STATUS", Width: 12},
		},
		List:       listShares,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "backend"), iStr(r["size_gb"]), s(r, "project_uuid"), s(r, "status")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteShare(ctx, &weftv1.DeleteShareRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "project", Label: "Project", Required: true},
			{Key: "name", Label: "Name", Required: true},
			{Key: "size_gb", Label: "Size (GB)", Required: true, Numeric: true},
			{Key: "backend", Label: "Backend (optional)", Placeholder: "cubefs"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			size, _ := strconv.Atoi(v["size_gb"])
			_, err := c.CreateShare(ctx, &weftv1.CreateShareRequest{
				Project: v["project"], Name: v["name"],
				SizeGb: int64(size), Backend: v["backend"],
			})
			if err != nil {
				return "", err
			}
			return "created share " + v["name"], nil
		},
	},
	{
		// Collections = iRODS collection paths (the iRODS equivalent
		// of directories : group DataObjects + sub-collections under
		// a logical path like /tempZone/home/alice/data). Listed via
		// the weft-ha-irods plugin once its proxy RPC ships ; for
		// now listCollections returns no rows and the table renders
		// empty — discoverable from the sidebar without erroring out.
		ID: "collections", Title: "Collections", Section: "Storage",
		// Gate on the iRODS HA plugin. Operators see this entry only
		// when at least one `irods-ha` instance is installed in the
		// cluster (catalogue name is `irods-ha` — the binary inside
		// is weft-ha-irods, but the catalogue entry is the shorter
		// form per catalogue/irods-ha/plugin.hcl).
		RequiresPlugin: "irods-ha",
		Columns: []table.Column{
			{Title: "PATH", Width: 40}, {Title: "OWNER", Width: 16},
			{Title: "ZONE", Width: 14}, {Title: "OBJECTS", Width: 10},
			{Title: "CREATED", Width: 20},
		},
		List: listCollections,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "path"), s(r, "owner"), s(r, "zone"), iStr(r["objects"]), s(r, "created_at")}
		},
		// No Actions / CreateFields until the iRODS RPC lands — the
		// data plane lives on the iRODS side, so create/delete here
		// would need a round-trip the agent doesn't yet expose.
	},
	{
		ID: "buckets", Title: "Buckets", Section: "Storage",
		// S3-compatible bucket backend in openweft = CubeFS objectnode
		// (cf. [feedback_no_minio]). Gated like Shares — same plugin
		// hosts both surfaces.
		RequiresPlugin: "cubefs",
		Columns: []table.Column{
			{Title: "NAME", Width: 22}, {Title: "ENDPOINT", Width: 32},
			{Title: "REGION", Width: 14}, {Title: "PROJECT", Width: 18},
		},
		List:       listBuckets,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "endpoint"), s(r, "region"), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteBucket(ctx, &weftv1.DeleteBucketRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "project", Label: "Project", Required: true},
			{Key: "name", Label: "Name", Required: true},
			{Key: "endpoint", Label: "Endpoint URL", Placeholder: "https://s3.example.com", Required: true},
			{Key: "region", Label: "Region", Placeholder: "us-east-1"},
			{Key: "access_key_id", Label: "Access key ID", Required: true},
			{Key: "secret_access_key", Label: "Secret access key", Required: true},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.CreateBucket(ctx, &weftv1.CreateBucketRequest{
				Project: v["project"], Name: v["name"],
				Endpoint: v["endpoint"], Region: v["region"],
				AccessKeyId: v["access_key_id"], SecretAccessKey: v["secret_access_key"],
			})
			if err != nil {
				return "", err
			}
			return "created bucket " + v["name"], nil
		},
	},
	{
		ID: "floating-ips", Title: "Floating IPs", Section: "Network",
		Columns: []table.Column{
			{Title: "ADDRESS", Width: 16},
			{Title: "NETWORK_UUID", Width: 36},
			{Title: "MAPPED-TO", Width: 22},
			{Title: "PROJECT_UUID", Width: 36},
		},
		List:       listFloatingIPs,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "address"), s(r, "network_uuid"), s(r, "mapped_to_uuid"), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "release", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.ReleaseFloatingIP(ctx, &weftv1.ReleaseFloatingIPRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "project", Label: "Project", Required: true},
			{Key: "network", Label: "Edge network (name or UUID)", Required: true},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.AllocateFloatingIP(ctx, &weftv1.AllocateFloatingIPRequest{
				Project: v["project"], Network: v["network"],
			})
			if err != nil {
				return "", err
			}
			return "allocated floating IP on " + v["network"], nil
		},
	},
	{
		ID: "loadbalancers", Title: "Load Balancers", Section: "Network",
		Columns: []table.Column{
			{Title: "NAME", Width: 20}, {Title: "FRONTEND", Width: 22},
			{Title: "BACKENDS", Width: 10}, {Title: "PROJECT", Width: 18},
		},
		List:       listLoadBalancers,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "frontend"), iStr(r["backends_count"]), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteLoadBalancer(ctx, &weftv1.DeleteLoadBalancerRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "project", Label: "Project", Required: true},
			{Key: "name", Label: "Name", Required: true},
			{Key: "listen_addr", Label: "Listen address", Placeholder: ":80", Required: true},
			{Key: "protocol", Label: "Protocol", Placeholder: "tcp | http", Required: true},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.CreateLoadBalancer(ctx, &weftv1.CreateLoadBalancerRequest{
				Project: v["project"], Name: v["name"],
				ListenAddr: v["listen_addr"], Protocol: v["protocol"],
			})
			if err != nil {
				return "", err
			}
			return "created load balancer " + v["name"], nil
		},
		EditFields: []FormField{
			{Key: "name", Label: "Name (empty=keep current)"},
			{Key: "listen_addr", Label: "Listen addr (empty=keep current)"},
			{Key: "protocol", Label: "Protocol (empty=keep current)"},
		},
		EditFn: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any, v map[string]string) (string, error) {
			uuid := s(row, "uuid")
			_, err := c.UpdateLoadBalancer(ctx, &weftv1.UpdateLoadBalancerRequest{
				Uuid: uuid, Name: v["name"],
				ListenAddr: v["listen_addr"], Protocol: v["protocol"],
			})
			if err != nil {
				return "", err
			}
			return "updated load balancer " + s(row, "name"), nil
		},
	},
	{
		ID: "dns-zones", Title: "DNS Zones", Section: "Network",
		Columns: []table.Column{
			{Title: "NAME", Width: 30}, {Title: "TTL", Width: 8},
			{Title: "PROJECT", Width: 18},
		},
		List:       listDNSZones,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), iStr(r["default_ttl"]), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteDNSZone(ctx, &weftv1.DeleteDNSZoneRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "project", Label: "Project", Required: true},
			{Key: "name", Label: "Zone name", Placeholder: "example.com.", Required: true},
			{Key: "soa_email", Label: "SOA email", Placeholder: "hostmaster@example.com"},
			{Key: "ttl", Label: "Default TTL (seconds)", Placeholder: "3600", Numeric: true},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			ttl, _ := strconv.Atoi(v["ttl"])
			_, err := c.CreateDNSZone(ctx, &weftv1.CreateDNSZoneRequest{
				Project: v["project"], Name: v["name"],
				SoaEmail: v["soa_email"], Ttl: int32(ttl),
			})
			if err != nil {
				return "", err
			}
			return "created DNS zone " + v["name"], nil
		},
		EditFields: []FormField{
			{Key: "soa_email", Label: "SOA email (empty=keep current)"},
			{Key: "ttl", Label: "Default TTL (-1=keep current)", Numeric: true},
		},
		EditFn: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any, v map[string]string) (string, error) {
			uuid := s(row, "uuid")
			ttl := -1
			if t, _ := strconv.Atoi(v["ttl"]); v["ttl"] != "" {
				ttl = t
			}
			_, err := c.UpdateDNSZone(ctx, &weftv1.UpdateDNSZoneRequest{
				Uuid: uuid, SoaEmail: v["soa_email"], Ttl: int32(ttl),
			})
			if err != nil {
				return "", err
			}
			return "updated DNS zone " + s(row, "name"), nil
		},
	},
	{
		ID: "dns-records", Title: "DNS Records", Section: "Network",
		Columns: []table.Column{
			{Title: "NAME", Width: 22}, {Title: "TYPE", Width: 8},
			{Title: "VALUE", Width: 30}, {Title: "ZONE", Width: 22},
		},
		List:       listDNSRecords,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "type"), s(r, "value"), s(r, "zone_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteDNSRecord(ctx, &weftv1.DeleteDNSRecordRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "zone_uuid", Label: "Zone UUID", Required: true},
			{Key: "name", Label: "Record name", Placeholder: "www", Required: true},
			{Key: "type", Label: "Type", Placeholder: "A | AAAA | CNAME | MX | TXT", Required: true},
			{Key: "value", Label: "Value", Required: true},
			{Key: "ttl", Label: "TTL (seconds)", Placeholder: "300", Numeric: true},
			{Key: "priority", Label: "Priority (MX only)", Numeric: true},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			ttl, _ := strconv.Atoi(v["ttl"])
			prio, _ := strconv.Atoi(v["priority"])
			_, err := c.CreateDNSRecord(ctx, &weftv1.CreateDNSRecordRequest{
				ZoneUuid: v["zone_uuid"], Name: v["name"],
				Type: v["type"], Value: v["value"],
				Ttl: int32(ttl), Priority: int32(prio),
			})
			if err != nil {
				return "", err
			}
			return "created DNS record " + v["name"], nil
		},
		EditFields: []FormField{
			{Key: "value", Label: "Value (empty=keep current)"},
			{Key: "ttl", Label: "TTL (-1=keep current)", Numeric: true},
			{Key: "priority", Label: "Priority (-1=keep current)", Numeric: true},
		},
		EditFn: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any, v map[string]string) (string, error) {
			uuid := s(row, "uuid")
			ttl := -1
			if t, _ := strconv.Atoi(v["ttl"]); v["ttl"] != "" {
				ttl = t
			}
			prio := -1
			if p, _ := strconv.Atoi(v["priority"]); v["priority"] != "" {
				prio = p
			}
			_, err := c.UpdateDNSRecord(ctx, &weftv1.UpdateDNSRecordRequest{
				Uuid: uuid, Value: v["value"],
				Ttl: int32(ttl), Priority: int32(prio),
			})
			if err != nil {
				return "", err
			}
			return "updated DNS record " + s(row, "name"), nil
		},
	},
	{
		ID: "security-groups", Title: "Security Groups", Section: "Network",
		Columns: []table.Column{
			{Title: "NAME", Width: 22}, {Title: "DESCRIPTION", Width: 32},
			{Title: "PROJECT", Width: 18},
		},
		List:       listSecurityGroups,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "description"), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteSecurityGroup(ctx, &weftv1.DeleteSecurityGroupRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "project", Label: "Project", Required: true},
			{Key: "name", Label: "Name", Required: true},
			{Key: "description", Label: "Description"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.CreateSecurityGroup(ctx, &weftv1.CreateSecurityGroupRequest{
				Project: v["project"], Name: v["name"],
				Description: v["description"],
			})
			if err != nil {
				return "", err
			}
			return "created security group " + v["name"], nil
		},
		EditFields: []FormField{
			{Key: "description", Label: "Description (empty=keep current)"},
		},
		EditFn: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any, v map[string]string) (string, error) {
			uuid := s(row, "uuid")
			_, err := c.SetSecurityGroupDescription(ctx, &weftv1.SetSecurityGroupDescriptionRequest{
				Uuid: uuid, Description: v["description"],
			})
			if err != nil {
				return "", err
			}
			return "updated security group " + s(row, "name"), nil
		},
	},
	{
		ID: "scheduling-rules", Title: "Scheduling Rules", Section: "Compute",
		Columns: []table.Column{
			{Title: "NAME", Width: 24}, {Title: "SELECTOR", Width: 28},
			{Title: "TARGET", Width: 8},
		},
		List:       listSchedulingRules,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "selector"), iStr(r["target_count"])}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteSchedulingRule(ctx, &weftv1.DeleteSchedulingRuleRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "name", Label: "Name", Required: true},
			{Key: "selector", Label: "Selector", Placeholder: "tier=edge,role=compute", Required: true},
			{Key: "target_count", Label: "Target count", Required: true, Numeric: true},
			{Key: "anti_affinity", Label: "Anti-affinity", Placeholder: "host | az | rack"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			n, _ := strconv.Atoi(v["target_count"])
			_, err := c.CreateSchedulingRule(ctx, &weftv1.CreateSchedulingRuleRequest{
				Name: v["name"], Selector: v["selector"],
				TargetCount: int32(n), AntiAffinity: v["anti_affinity"],
			})
			if err != nil {
				return "", err
			}
			return "created scheduling rule " + v["name"], nil
		},
	},
	{
		ID: "tenants", Title: "Tenants", Section: "Identity",
		Columns: []table.Column{
			{Title: "NAME", Width: 20},
			{Title: "DOMAIN", Width: 22},
			{Title: "STATUS", Width: 10},
			{Title: "ADMINS", Width: 7},
			{Title: "MEMBERS", Width: 8},
			{Title: "VMS", Width: 6},
		},
		List: listTenants,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "domain"), s(r, "status"), iStr(r["admins_count"]), iStr(r["members_count"]), iStr(r["vms_count"])}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteTenant(ctx, &weftv1.DeleteTenantRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "name", Label: "Name", Required: true},
			{Key: "domain", Label: "Domain (e.g. acme.example.com)", Placeholder: "acme.example.com"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.CreateTenant(ctx, &weftv1.CreateTenantRequest{
				Name: v["name"], Domain: v["domain"],
			})
			if err != nil {
				return "", err
			}
			return "created tenant " + v["name"], nil
		},
	},
	{
		ID: "users", Title: "Users", Section: "Identity",
		Columns: []table.Column{
			{Title: "EMAIL", Width: 30}, {Title: "DISPLAY", Width: 22},
			{Title: "ISSUER", Width: 24},
		},
		List:       listUsers,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "email"), s(r, "display_name"), s(r, "oidc_issuer")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteUser(ctx, &weftv1.DeleteUserRequest{Uuid: uuid})
				return err
			})},
		},
	},
	{
		ID: "sshkeys", Title: "SSH Keys (catalogue)", Section: "Identity",
		Columns: []table.Column{
			{Title: "NAME", Width: 22}, {Title: "FINGERPRINT", Width: 50}, {Title: "SOURCE", Width: 12},
		},
		List:       listSSHKeyCatalogue,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "fingerprint"), s(r, "source")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "remove", Confirm: "yes", Do: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
				_, err := c.RemoveSSHKeyCatalogue(ctx, &weftv1.RemoveSSHKeyCatalogueRequest{Name: s(row, "name")})
				if err != nil {
					return "", err
				}
				return "removed " + s(row, "name"), nil
			}},
		},
		CreateFields: []FormField{
			{Key: "name", Label: "Name (unique cluster-wide)", Required: true},
			{Key: "public_key", Label: "Public key (OpenSSH single-line)", Required: true},
			{Key: "comment", Label: "Comment (optional)"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.AddSSHKeyCatalogue(ctx, &weftv1.AddSSHKeyCatalogueRequest{
				Name: v["name"], PublicKey: v["public_key"], Comment: v["comment"],
			})
			if err != nil {
				return "", err
			}
			return "added ssh key " + v["name"], nil
		},
	},
	{
		ID: "flavors", Title: "Flavors", Section: "Compute",
		Columns: []table.Column{
			{Title: "NAME", Width: 18},
			{Title: "VCPU", Width: 6},
			{Title: "RAM-GIB", Width: 8},
			{Title: "GPU", Width: 16},
			{Title: "VMS", Width: 5},
			{Title: "UUID", Width: 36},
		},
		List: listFlavors,
		RowToCells: func(r map[string]any) []string {
			return []string{
				s(r, "name"),
				iStr(r["vcpu"]),
				iStr(r["ram_gib"]),
				s(r, "gpu"),
				iStr(r["vm_count"]),
				s(r, "uuid"),
			}
		},
		// Flavors mutate via SetFlavor / DeleteFlavor — they're
		// cluster-admin only and rare ; the TUI surfaces read-only.
	},
	{
		ID: "azs", Title: "Availability Zones", Section: "Admin",
		Columns: []table.Column{
			{Title: "CODE", Width: 8},
			{Title: "NAME", Width: 18},
			{Title: "STATUS", Width: 10},
			{Title: "UUID", Width: 36},
		},
		List: listAZs,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "code"), s(r, "name"), s(r, "status"), s(r, "uuid")}
		},
		Actions: []ResourceAction{
			{Key: "a", Label: "activate", Do: setAZStatus("active")},
			{Key: "i", Label: "inactivate", Do: setAZStatus("inactive")},
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteAZ(ctx, &weftv1.DeleteAZRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "code", Label: "Code", Placeholder: "dc1", Required: true},
			{Key: "name", Label: "Name", Required: true},
			{Key: "region", Label: "Region", Placeholder: "fr-paris"},
			{Key: "status", Label: "Status", Placeholder: "active"},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			_, err := c.CreateAZ(ctx, &weftv1.CreateAZRequest{
				Code: v["code"], Name: v["name"],
				Region: v["region"], Status: v["status"],
			})
			if err != nil {
				return "", err
			}
			return "created AZ " + v["code"], nil
		},
		EditFields: []FormField{
			{Key: "name", Label: "Name"},
			{Key: "region", Label: "Region"},
			{Key: "status", Label: "Status"},
		},
		EditFn: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any, v map[string]string) (string, error) {
			uuid := s(row, "uuid")
			_, err := c.UpdateAZ(ctx, &weftv1.UpdateAZRequest{
				Uuid: uuid, Name: v["name"],
				Region: v["region"], Status: v["status"],
			})
			if err != nil {
				return "", err
			}
			return "updated AZ " + s(row, "code"), nil
		},
	},
	{
		ID: "racks", Title: "Racks", Section: "Admin",
		Columns: []table.Column{
			{Title: "NAME", Width: 14},
			{Title: "AZ_NAME", Width: 16},
			{Title: "STATUS", Width: 10},
			{Title: "HOSTS", Width: 6},
			{Title: "UUID", Width: 36},
			{Title: "AZ_UUID", Width: 36},
		},
		List: listRacks,
		RowToCells: func(r map[string]any) []string {
			// NAME = "<az_code>:<rack_code>" so racks named r1 in
			// different AZs stay distinguishable at a glance.
			// AZ_NAME = the AZ's human-friendly display name (e.g.
			// "DC 1" vs code "dc1"). Operator directive 2026-06-24.
			name := s(r, "code")
			if az := s(r, "az_code"); az != "" {
				name = az + ":" + name
			}
			return []string{name, s(r, "az_name"), s(r, "status"), iStr(r["position"]), s(r, "uuid"), s(r, "az_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "a", Label: "activate", Do: setRackStatus("active")},
			{Key: "i", Label: "inactivate", Do: setRackStatus("inactive")},
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteRack(ctx, &weftv1.DeleteRackRequest{Uuid: uuid})
				return err
			})},
		},
		CreateFields: []FormField{
			{Key: "az_uuid", Label: "AZ UUID", Required: true},
			{Key: "code", Label: "Code", Placeholder: "r1", Required: true},
			{Key: "name", Label: "Name"},
			{Key: "status", Label: "Status", Placeholder: "active"},
			{Key: "height_u", Label: "Height (U)", Placeholder: "42", Numeric: true},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			h, _ := strconv.Atoi(v["height_u"])
			_, err := c.CreateRack(ctx, &weftv1.CreateRackRequest{
				AzUuid: v["az_uuid"], Code: v["code"], Name: v["name"],
				Status: v["status"], HeightU: int32(h),
			})
			if err != nil {
				return "", err
			}
			return "created rack " + v["code"], nil
		},
		EditFields: []FormField{
			{Key: "name", Label: "Name"},
			{Key: "status", Label: "Status"},
			{Key: "height_u", Label: "Height U (-1=keep current)", Numeric: true},
		},
		EditFn: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any, v map[string]string) (string, error) {
			uuid := s(row, "uuid")
			h := -1
			if hh, _ := strconv.Atoi(v["height_u"]); v["height_u"] != "" {
				h = hh
			}
			_, err := c.UpdateRack(ctx, &weftv1.UpdateRackRequest{
				Uuid: uuid, Name: v["name"],
				Status: v["status"], HeightU: int32(h),
			})
			if err != nil {
				return "", err
			}
			return "updated rack " + s(row, "code"), nil
		},
	},
	{
		ID: "images", Title: "Images", Section: "Storage",
		Columns: []table.Column{
			{Title: "URL", Width: 50}, {Title: "FORMAT", Width: 10}, {Title: "SIZE", Width: 12},
		},
		List:       listImages,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "url"), s(r, "format"), iStr(r["size_bytes"])}
		},
	},
	{
		ID: "plugins", Title: "Installed Plugins", Section: "Admin",
		Columns: []table.Column{
			{Title: "NAME", Width: 22}, {Title: "VERSION", Width: 14},
			{Title: "STATE", Width: 12}, {Title: "PROJECT", Width: 18},
		},
		List:       listInstalledPlugins,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "version"), s(r, "state"), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{
				Key:     "i",
				Label:   "install",
				Confirm: "yes",
				Do: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
					name := s(row, "name")
					if name == "" {
						return "", fmt.Errorf("row has no plugin name")
					}
					if s(row, "state") != "available" {
						return "", fmt.Errorf("plugin %q is already installed (state=%s)", name, s(row, "state"))
					}
					// Project resolution : V1 installs into the default
					// project. The catalogue entry may declare required
					// inputs but the TUI doesn't surface a per-input
					// form here yet — the agent's defaults cover the
					// happy path for irods-ha / cubefs / weft-webui.
					// CreateFields-driven prompting comes with the
					// CreateFn flow when the gating UX expands beyond
					// this Action.
					resp, err := c.InstallPlugin(ctx, &weftv1.InstallPluginRequest{Name: name})
					if err != nil {
						return "", err
					}
					if resp.InstanceUuid == "" {
						return "installed " + name + " (no instance uuid returned)", nil
					}
					return "installed " + name + " (instance=" + resp.InstanceUuid + ")", nil
				},
			},
		},
	},
	{
		ID: "volume-snapshots", Title: "Volume Snapshots", Section: "Storage",
		// Volume snapshots only matter once the block backend exists.
		RequiresPlugin: "weft-block",
		Columns: []table.Column{
			{Title: "NAME", Width: 24}, {Title: "VOLUME", Width: 22},
			{Title: "SIZE-GIB", Width: 10}, {Title: "PROJECT", Width: 18},
		},
		List:       listVolumeSnapshots,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "volume_uuid"), iStr(r["size_gib"]), s(r, "project_uuid")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteVolumeSnapshot(ctx, &weftv1.DeleteVolumeSnapshotRequest{Uuid: uuid})
				return err
			})},
		},
	},
	{
		ID: "volume-backups", Title: "Volume Backups", Section: "Storage",
		// Same gate as volumes / volume-snapshots — without the block
		// backend there's nothing to back up.
		RequiresPlugin: "weft-block",
		Columns: []table.Column{
			{Title: "NAME", Width: 24}, {Title: "VOLUME", Width: 22},
			{Title: "SIZE-GIB", Width: 10}, {Title: "STATUS", Width: 12},
		},
		List:       listVolumeBackups,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "volume_uuid"), iStr(r["size_gib"]), s(r, "status")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
				url := s(row, "url")
				if url == "" {
					return "", fmt.Errorf("row has no url column")
				}
				if _, err := c.DeleteVolumeBackup(ctx, &weftv1.DeleteVolumeBackupRequest{Url: url}); err != nil {
					return "", err
				}
				return "deleted " + url, nil
			}},
		},
	},
}

// resourceByID looks up a config by slug. Returns (nil, false) when
// the operator typed a name we don't recognise.
func resourceByID(id string) (ResourceConfig, bool) {
	for _, r := range resourceCatalogue {
		if r.ID == id {
			return r, true
		}
	}
	return ResourceConfig{}, false
}

// ---------------- helpers -------------------------------------------

func s(r map[string]any, key string) string {
	if r == nil {
		return ""
	}
	if v, ok := r[key]; ok {
		if str, ok := v.(string); ok {
			return str
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func iStr(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case int:
		return strconv.Itoa(n)
	case int32:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case uint32:
		return strconv.FormatUint(uint64(n), 10)
	case uint64:
		return strconv.FormatUint(n, 10)
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", n)
	}
}

// deleteByUUID is a thin shim around a per-noun Delete RPC : extracts
// the "uuid" field from the row, calls the closure, returns a
// formatted success message.
func deleteByUUID(rpc func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error) func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
	return func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
		uuid := s(row, "uuid")
		if uuid == "" {
			return "", fmt.Errorf("row has no uuid column")
		}
		if err := rpc(ctx, c, uuid); err != nil {
			return "", err
		}
		return "deleted " + uuid, nil
	}
}

// setAZStatus + setRackStatus return ResourceAction.Do closures
// that flip the inventory record's status field via UpdateAZ /
// UpdateRack with all the other fields left as "" (= keep current).
// Operator directive 2026-06-24 : "il faudrait ajouter la
// possibilité de passer une AZ, un rack, un host en statut
// inactif/actif". Host is already covered by `c`/`u`/`d` on the
// hosts table — the cordon/uncordon/down handlers in hosts.go.
func setAZStatus(status string) func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
	return func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
		uuid := s(row, "uuid")
		if uuid == "" {
			return "", fmt.Errorf("row has no uuid column")
		}
		_, err := c.UpdateAZ(ctx, &weftv1.UpdateAZRequest{
			Uuid:   uuid,
			Status: status,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("az %s → %s", s(row, "code"), status), nil
	}
}

func setRackStatus(status string) func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
	return func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
		uuid := s(row, "uuid")
		if uuid == "" {
			return "", fmt.Errorf("row has no uuid column")
		}
		_, err := c.UpdateRack(ctx, &weftv1.UpdateRackRequest{
			Uuid:   uuid,
			Status: status,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("rack %s → %s", s(row, "code"), status), nil
	}
}

// ctxWithTimeout is a 5s default context for list calls — keeps the
// UI responsive when the agent is slow.
func ctxWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
