package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	compute "google.golang.org/api/compute/v1"
)

// InstanceTemplateLister is the small seam behind the GCP custom-image
// reference pass. It exists so the provider never has to hand-roll pagination
// or auth around instanceTemplates.list, and so tests can replay template
// references offline without a Compute Engine client.
type InstanceTemplateLister interface {
	// ListSourceImages returns every sourceImage referenced by an instance
	// template in project, in API order. It is the caller's job to resolve
	// those strings (self-links, partial URLs, bare image names, or family
	// references) against the project's custom images.
	ListSourceImages(ctx context.Context, project string) ([]string, error)
}

// computeInstanceTemplateLister is the live implementation backed by the
// generated Compute Engine API client. The official SDK handles auth, retries
// and pagination; this type only projects the fields the reference pass needs.
type computeInstanceTemplateLister struct {
	svc *compute.Service
	log *slog.Logger
}

var _ InstanceTemplateLister = (*computeInstanceTemplateLister)(nil)

// newComputeInstanceTemplateLister builds a live Compute Engine client. It is
// deliberately constructed lazily during ingest, only when an image rule
// actually asked for compute.googleapis.com/Image, so existing scans do not
// need instanceTemplates.list permission.
func newComputeInstanceTemplateLister(ctx context.Context, log *slog.Logger) (*computeInstanceTemplateLister, error) {
	if log == nil {
		log = slog.Default()
	}
	svc, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: create Compute Engine client for instance template references (check Application Default Credentials): %w", err)
	}
	return &computeInstanceTemplateLister{svc: svc, log: log}, nil
}

// ListSourceImages pages through instanceTemplates.list and extracts
// properties.disks[].initializeParams.sourceImage. Only sourceImage is
// collected; sourceSnapshot is deliberately ignored because a snapshot is not
// an image reference.
func (l *computeInstanceTemplateLister) ListSourceImages(ctx context.Context, project string) ([]string, error) {
	var refs []string
	call := l.svc.InstanceTemplates.List(project)
	err := call.Pages(ctx, func(list *compute.InstanceTemplateList) error {
		if list == nil {
			return nil
		}
		for _, tmpl := range list.Items {
			if tmpl == nil || tmpl.Properties == nil {
				continue
			}
			for _, disk := range tmpl.Properties.Disks {
				if disk == nil || disk.InitializeParams == nil || disk.InitializeParams.SourceImage == "" {
					continue
				}
				refs = append(refs, disk.InitializeParams.SourceImage)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: list instance templates for project %s: %w", project, err)
	}
	return refs, nil
}

// enrichImageReferences runs the instance-template reference pass for every
// project that produced at least one custom-image node. It is called only
// when the scan's asset-type hints contain TypeImage; otherwise no Compute
// Engine client is built and no template-list permission is required.
func (p *Provider) enrichImageReferences(ctx context.Context, byProject map[string][]*graph.Node) {
	if len(byProject) == 0 {
		return
	}

	lister := p.templates
	if lister == nil && !p.offline {
		l, err := newComputeInstanceTemplateLister(ctx, p.log)
		if err != nil {
			p.log.Warn("gcp: instance template reference enumeration unavailable; custom image rules will skip",
				"err", err)
		} else {
			lister = l
		}
	}

	for project, nodes := range byProject {
		if lister == nil {
			setImageReferencesComplete(nodes, false)
			p.log.Warn("gcp: no instance template lister available; custom image references are incomplete",
				"project", project)
			continue
		}
		refs, err := lister.ListSourceImages(ctx, project)
		if err != nil {
			p.log.Warn("gcp: instance template reference enumeration failed; custom image rules will skip",
				"project", project, "err", err)
			setImageReferencesComplete(nodes, false)
			continue
		}
		if !applyTemplateReferences(project, nodes, refs) {
			p.log.Warn("gcp: malformed instance template image reference; custom image rules will skip",
				"project", project)
			setImageReferencesComplete(nodes, false)
			continue
		}
		setImageReferencesComplete(nodes, true)
	}
}

// setImageReferencesComplete stamps every custom image in one project with the
// outcome of the template-reference pass.
func setImageReferencesComplete(nodes []*graph.Node, complete bool) {
	for _, n := range nodes {
		n.SetAttr(AttrReferencesComplete, complete)
	}
}

// applyTemplateReferences resolves every template sourceImage against the
// project's custom-image nodes and writes reference_count and
// reference_sources. It returns false when a reference string is malformed
// (the design treats malformed references as incomplete enumeration, never as
// proof of "zero references").
func applyTemplateReferences(project string, nodes []*graph.Node, refs []string) bool {
	byID := map[string]*graph.Node{}
	families := map[string][]*graph.Node{}
	for _, n := range nodes {
		byID[string(n.ID)] = n
		if family, ok := n.Str(AttrFamily); ok && family != "" {
			families[family] = append(families[family], n)
		}
	}

	counts := map[*graph.Node]int{}
	sources := map[*graph.Node]map[string]bool{}

	for _, ref := range refs {
		targets, ok := resolveImageReference(project, ref, byID, families)
		if !ok {
			return false
		}
		for _, n := range targets {
			counts[n]++
			if sources[n] == nil {
				sources[n] = map[string]bool{}
			}
			sources[n]["instance_template"] = true
		}
	}

	for _, n := range nodes {
		n.SetAttr(AttrReferenceCount, float64(counts[n]))
		labels := make([]string, 0, len(sources[n]))
		for label := range sources[n] {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		n.SetAttr(AttrReferenceSources, labels)
	}
	return true
}

// resolveImageReference resolves one template sourceImage string to the custom
// images it references in the template's project. ok=false means the string is
// malformed and the whole project's reference pass must be treated as
// incomplete.
func resolveImageReference(project, ref string, byID map[string]*graph.Node, families map[string][]*graph.Node) ([]*graph.Node, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, true
	}

	path, ok := imageReferencePath(ref, project)
	if !ok {
		return nil, false
	}
	if path == "" {
		return nil, true
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "projects" || parts[1] != project {
		// A public/other-project image reference is not one of this project's
		// custom images; it is valid and simply does not reference any node
		// here.
		return nil, true
	}

	const marker = "global/images/"
	idx := strings.Index(path, marker)
	if idx < 0 {
		return nil, true
	}
	tail := path[idx+len(marker):]
	if tail == "" {
		return nil, false
	}

	// Family reference: projects/<p>/global/images/family/<family>. An
	// instance template pointing at a family can resolve to ANY member, so
	// every custom image in that family is in use.
	if strings.HasPrefix(tail, "family/") {
		family := strings.TrimPrefix(tail, "family/")
		family = strings.TrimSuffix(family, "/")
		if family == "" {
			return nil, false
		}
		return families[family], true
	}

	// Specific image reference. A relative global/images/<name> reference was
	// already resolved against the template's project in imageReferencePath.
	imageName := strings.TrimSuffix(tail, "/")
	if imageName == "" {
		return nil, false
	}
	assetName := "//compute.googleapis.com/projects/" + project + "/global/images/" + imageName
	if n, ok := byID[assetName]; ok {
		return []*graph.Node{n}, true
	}
	return nil, true
}

// imageReferencePath normalizes the many sourceImage spellings Compute Engine
// accepts into the resource path form used by the resolver:
//
//	https://www.googleapis.com/compute/v1/projects/p/global/images/i
//	projects/p/global/images/i
//	global/images/i
//	i
//	global/images/family/f
//	family/f
func imageReferencePath(ref, project string) (string, bool) {
	switch {
	case strings.HasPrefix(ref, "//"):
		path := strings.TrimPrefix(NormalizeSelfLink(ref), "//compute.googleapis.com/")
		if path == "" {
			return "", false
		}
		return path, true
	case strings.Contains(ref, "://"):
		path := strings.TrimPrefix(NormalizeSelfLink(ref), "//compute.googleapis.com/")
		if path == "" {
			return "", false
		}
		return path, true
	case strings.HasPrefix(ref, "projects/"):
		return ref, true
	case strings.HasPrefix(ref, "global/images/"):
		return "projects/" + project + "/" + ref, true
	case strings.HasPrefix(ref, "family/"):
		return "projects/" + project + "/global/images/" + ref, true
	default:
		// Compute accepts a bare custom image name in instance template disk
		// initialize params; resolve it against the template's project.
		return "projects/" + project + "/global/images/" + ref, true
	}
}
