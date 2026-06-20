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
	},
	{
		ID: "subnets", Title: "Subnets", Section: "Network",
		Columns: []table.Column{
			{Title: "NAME", Width: 18}, {Title: "CIDR", Width: 18},
			{Title: "NETWORK", Width: 18}, {Title: "PROJECT", Width: 18},
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
	},
	{
		ID: "volumes", Title: "Volumes", Section: "Storage",
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
	},
	{
		ID: "shares", Title: "Shares", Section: "Storage",
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
	},
	{
		ID: "buckets", Title: "Buckets", Section: "Storage",
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
	},
	{
		ID: "floating-ips", Title: "Floating IPs", Section: "Network",
		Columns: []table.Column{
			{Title: "ADDRESS", Width: 18}, {Title: "NETWORK", Width: 18},
			{Title: "MAPPED-TO", Width: 22}, {Title: "PROJECT", Width: 18},
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
	},
	{
		ID: "tenants", Title: "Tenants", Section: "Identity",
		Columns: []table.Column{
			{Title: "NAME", Width: 22}, {Title: "DOMAIN", Width: 22},
			{Title: "STATUS", Width: 12}, {Title: "ADMINS", Width: 8}, {Title: "MEMBERS", Width: 8},
		},
		List:       listTenants,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), s(r, "domain"), s(r, "status"), iStr(r["admins_count"]), iStr(r["members_count"])}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteTenant(ctx, &weftv1.DeleteTenantRequest{Uuid: uuid})
				return err
			})},
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
	},
	{
		ID: "flavors", Title: "Flavors", Section: "Compute",
		Columns: []table.Column{
			{Title: "NAME", Width: 18}, {Title: "VCPU", Width: 6},
			{Title: "RAM-GIB", Width: 8}, {Title: "GPU", Width: 16},
		},
		List:       listFlavors,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "name"), iStr(r["vcpu"]), iStr(r["ram_gib"]), s(r, "gpu")}
		},
		// Flavors mutate via SetFlavor / DeleteFlavor — they're
		// cluster-admin only and rare ; the TUI surfaces read-only.
	},
	{
		ID: "azs", Title: "Availability Zones", Section: "Admin",
		Columns: []table.Column{
			{Title: "CODE", Width: 12}, {Title: "NAME", Width: 22}, {Title: "STATUS", Width: 12},
		},
		List:       listAZs,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "code"), s(r, "name"), s(r, "status")}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteAZ(ctx, &weftv1.DeleteAZRequest{Uuid: uuid})
				return err
			})},
		},
	},
	{
		ID: "racks", Title: "Racks", Section: "Admin",
		Columns: []table.Column{
			{Title: "CODE", Width: 12}, {Title: "AZ", Width: 12}, {Title: "POSITION", Width: 10},
		},
		List:       listRacks,
		RowToCells: func(r map[string]any) []string {
			return []string{s(r, "code"), s(r, "az_uuid"), iStr(r["position"])}
		},
		Actions: []ResourceAction{
			{Key: "d", Label: "delete", Confirm: "yes", Do: deleteByUUID(func(ctx context.Context, c weftv1.WeftAgentClient, uuid string) error {
				_, err := c.DeleteRack(ctx, &weftv1.DeleteRackRequest{Uuid: uuid})
				return err
			})},
		},
	},
	{
		ID: "images", Title: "Images", Section: "Admin",
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
	},
	{
		ID: "volume-snapshots", Title: "Volume Snapshots", Section: "Storage",
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

// ctxWithTimeout is a 5s default context for list calls — keeps the
// UI responsive when the agent is slow.
func ctxWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
