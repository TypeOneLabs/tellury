package gcp

import (
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// linker extracts edges from one asset. It runs in the same pass as normalize;
// emitted edges may reference not-yet-seen nodes, and Freeze prunes leftovers.
//
// v0.1 linkers:
//
//	instance.disks[].source      → instance --attached_to--> disk
//	instance.networkInterfaces[] → instance --uses--> network
//	disk.users[]                 → instance --attached_to--> disk (deduped)
//	disk.sourceSnapshot/Image    → disk --created_from--> snapshot|image
type linker func(*RawAsset, func(graph.Edge)) error

var linkers = map[string][]linker{
	TypeInstance: {linkInstanceDisks, linkInstanceNetworks},
	TypeDisk:     {linkDiskUsers, linkDiskSource},
}

// Link emits every edge derivable from one asset.
func Link(a *RawAsset, emit func(graph.Edge)) error {
	if a == nil {
		return nil
	}
	for _, fn := range linkers[a.AssetType] {
		if err := fn(a, emit); err != nil {
			return err
		}
	}
	return nil
}

// linkInstanceDisks: instance --attached_to--> disk, from disks[].source.
func linkInstanceDisks(a *RawAsset, emit func(graph.Edge)) error {
	data := decodeData(a.Data())
	disks, ok := sliceOf(data["disks"])
	if !ok {
		return nil
	}
	for _, raw := range disks {
		d, ok := mapOf(raw)
		if !ok {
			continue
		}
		src, ok := strOf(d["source"])
		if !ok || src == "" {
			continue // local SSDs and scratch disks have no source
		}
		if ref := NormalizeSelfLink(src); ref != "" {
			emit(graph.Edge{From: graph.Ref(a.Name), To: graph.Ref(ref), Kind: graph.EdgeAttachedTo})
		}
	}
	return nil
}

// linkInstanceNetworks: instance --uses--> network.
func linkInstanceNetworks(a *RawAsset, emit func(graph.Edge)) error {
	data := decodeData(a.Data())
	nics, ok := sliceOf(data["networkInterfaces"])
	if !ok {
		return nil
	}
	for _, raw := range nics {
		nic, ok := mapOf(raw)
		if !ok {
			continue
		}
		for _, key := range []string{"network", "subnetwork"} {
			link, ok := strOf(nic[key])
			if !ok || link == "" {
				continue
			}
			if ref := NormalizeSelfLink(link); ref != "" {
				emit(graph.Edge{From: graph.Ref(a.Name), To: graph.Ref(ref), Kind: graph.EdgeUses})
			}
		}
	}
	return nil
}

// linkDiskUsers: instance --attached_to--> disk, from the disk's own users[].
//
// This is the single highest-value correctness detail in the ingestion layer: a
// disk attached to an instance OUTSIDE the scan scope still receives an inbound
// attached_to edge, so detached_disk does not report it as waste.
func linkDiskUsers(a *RawAsset, emit func(graph.Edge)) error {
	data := decodeData(a.Data())
	users, ok := sliceOf(data["users"])
	if !ok {
		return nil
	}
	for _, raw := range users {
		link, ok := strOf(raw)
		if !ok || link == "" {
			continue
		}
		if ref := NormalizeSelfLink(link); ref != "" {
			emit(graph.Edge{From: graph.Ref(ref), To: graph.Ref(a.Name), Kind: graph.EdgeAttachedTo})
		}
	}
	return nil
}

// linkDiskSource: disk --created_from--> snapshot|image.
func linkDiskSource(a *RawAsset, emit func(graph.Edge)) error {
	data := decodeData(a.Data())
	for _, key := range []string{"sourceSnapshot", "sourceImage"} {
		link, ok := strOf(data[key])
		if !ok || link == "" {
			continue
		}
		if ref := NormalizeSelfLink(link); ref != "" {
			emit(graph.Edge{From: graph.Ref(a.Name), To: graph.Ref(ref), Kind: graph.EdgeCreatedFrom})
		}
	}
	return nil
}
