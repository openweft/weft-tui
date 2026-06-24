package main

// catalogue_listers.go — one ListXxx wrapper per noun the
// catalogue.go entries consume. Each function takes the WeftAgent
// client + returns a flat []map[string]any the table widget
// renders.

import (
	"context"
	"fmt"

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
	out := make([]map[string]any, 0, len(resp.Tenants))
	for _, t := range resp.Tenants {
		out = append(out, map[string]any{
			"uuid": t.Uuid, "name": t.Name, "domain": t.Domain,
			"status":        t.Status,
			"admins_count":  t.Admins,
			"members_count": t.Members,
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

func listFlavors(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListFlavors(ctx, &weftv1.ListFlavorsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Flavors))
	for _, f := range resp.Flavors {
		out = append(out, map[string]any{
			"name": f.Name, "vcpu": f.Vcpu, "ram_gib": f.Ram,
			"gpu": f.Gpu,
		})
	}
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
	azByUUID := map[string]string{}
	if azResp, azErr := c.ListAZs(ctx, &weftv1.ListAZsRequest{}); azErr == nil {
		for _, a := range azResp.Azs {
			azByUUID[a.Uuid] = a.Code
		}
	}
	out := make([]map[string]any, 0, len(resp.Racks))
	for _, r := range resp.Racks {
		out = append(out, map[string]any{
			"uuid":     r.Uuid,
			"code":     r.Code,
			"status":   r.Status,
			"az_code":  azByUUID[r.AzUuid],
			"az_uuid":  r.AzUuid,
			"position": r.Hosts, // re-uses the position column slot to show host count
		})
	}
	return out, nil
}

func listImages(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListImages(ctx, &weftv1.ListImagesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Images))
	for _, img := range resp.Images {
		out = append(out, map[string]any{
			"url": img.Url, "name": img.Name,
			"format":     img.Format,
			"size_bytes": img.SizeBytes,
		})
	}
	return out, nil
}

func listInstalledPlugins(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
	resp, err := c.ListInstalledPlugins(ctx, &weftv1.ListInstalledPluginsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Instances))
	for _, p := range resp.Instances {
		out = append(out, map[string]any{
			"uuid":         p.InstanceUuid,
			"name":         p.Name,
			"version":      "", // PluginInstance has no per-install version on the wire
			"state":        p.Status,
			"project_uuid": p.Project,
		})
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
