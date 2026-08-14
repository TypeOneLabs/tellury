package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

type fakeTemplateLister struct {
	refs map[string][]string
	err  error
}

func (f fakeTemplateLister) ListSourceImages(_ context.Context, project string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.refs[project], nil
}

func imageRawAsset(name, id, family, status, creation, archiveBytes, location string) *RawAsset {
	return &RawAsset{
		Name:      "//compute.googleapis.com/projects/p/global/images/" + name,
		AssetType: TypeImage,
		Project:   "projects/p",
		Resource: &RawResource{
			Version:  "v1",
			Parent:   "//cloudresourcemanager.googleapis.com/projects/p",
			Location: location,
			Data: json.RawMessage(`{
				"name":"` + name + `",
				"id":"` + id + `",
				"family":"` + family + `",
				"status":"` + status + `",
				"creationTimestamp":"` + creation + `",
				"archiveSizeBytes":"` + archiveBytes + `",
				"storageLocations":["` + location + `"]
			}`),
		},
	}
}

func TestProviderImageReferencePass_InstanceTemplateAndFamily(t *testing.T) {
	assets := []*RawAsset{
		imageRawAsset("img-1", "1", "", "READY", "2024-01-01T00:00:00Z", "1073741824", "us-central1"),
		imageRawAsset("img-2", "2", "web", "READY", "2024-01-01T00:00:00Z", "1073741824", "us-central1"),
	}
	p, err := New(context.Background(),
		WithOffline(),
		WithLogger(newTestLogger()),
		WithLister(&FakeLister{Assets: assets}),
		WithInstanceTemplateLister(fakeTemplateLister{refs: map[string][]string{
			"p": {
				"https://www.googleapis.com/compute/v1/projects/p/global/images/img-1",
				"global/images/family/web",
			},
		}}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	g, err := p.Ingest(context.Background(), cloud.Scope{}, []string{TypeImage})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var img1, img2 *graph.Node
	g.ByKind(graph.KindImage, func(n *graph.Node) bool {
		switch n.Name {
		case "img-1":
			img1 = n
		case "img-2":
			img2 = n
		}
		return true
	})
	if img1 == nil || img2 == nil {
		t.Fatalf("images not normalized: img1=%v img2=%v", img1, img2)
	}

	for name, n := range map[string]*graph.Node{"img-1": img1, "img-2": img2} {
		complete, _ := n.Bool(AttrReferencesComplete)
		if !complete {
			t.Errorf("%s references_complete = false, want true", name)
		}
		cnt, _ := n.Num(AttrReferenceCount)
		if cnt != 1 {
			t.Errorf("%s reference_count = %v, want 1", name, cnt)
		}
	}

	sources := img1.Attrs[AttrReferenceSources].([]string)
	if len(sources) != 1 || sources[0] != "instance_template" {
		t.Errorf("img-1 reference_sources = %#v, want [instance_template]", sources)
	}
	famSources := img2.Attrs[AttrReferenceSources].([]string)
	if len(famSources) != 1 || famSources[0] != "instance_template" {
		t.Errorf("img-2 family reference_sources = %#v, want [instance_template]", famSources)
	}
}

func TestProviderImageReferencePass_TemplateListFailureMarksIncomplete(t *testing.T) {
	assets := []*RawAsset{
		imageRawAsset("img-1", "1", "", "READY", "2024-01-01T00:00:00Z", "1073741824", "us-central1"),
	}
	p, err := New(context.Background(),
		WithOffline(),
		WithLogger(newTestLogger()),
		WithLister(&FakeLister{Assets: assets}),
		WithInstanceTemplateLister(fakeTemplateLister{err: errors.New("permission denied")}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	g, err := p.Ingest(context.Background(), cloud.Scope{}, []string{TypeImage})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	g.ByKind(graph.KindImage, func(n *graph.Node) bool {
		complete, ok := n.Bool(AttrReferencesComplete)
		if !ok || complete {
			t.Errorf("references_complete = %v, %v; want false after template-list failure", complete, ok)
		}
		return true
	})
}
