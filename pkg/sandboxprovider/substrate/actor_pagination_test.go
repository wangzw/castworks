package substrate

import (
	"context"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
)

const paginationAtespace = "castworks-dev"

// actorReferencingTemplate returns the name of an actor in the atespace that
// still uses the template, or "" when none does. It must look at every page:
// the list is paginated and a reference on a later page still means the
// template cannot be deleted.
func actorReferencingTemplate(ctx context.Context, client ateapi.ControlClient, atespace, template string) (string, error) {
	var pageToken string
	for {
		resp, err := client.ListActors(ctx, &ateapi.ListActorsRequest{
			Atespace: atespace,
			Options:  &ateapi.ListOptions{PageToken: pageToken},
		})
		if err != nil {
			return "", err
		}
		for _, a := range resp.GetActors() {
			if a.GetActorTemplate().GetName() == template {
				return a.GetMetadata().GetName(), nil
			}
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return "", nil
		}
	}
}

// pagedActorsClient serves canned ListActors pages and records the page token
// each request carried, so a test can check that the whole list was traversed.
type pagedActorsClient struct {
	ateapi.ControlClient
	pages  []*ateapi.ListActorsResponse
	tokens []string
}

func (c *pagedActorsClient) ListActors(_ context.Context, in *ateapi.ListActorsRequest, _ ...grpc.CallOption) (*ateapi.ListActorsResponse, error) {
	c.tokens = append(c.tokens, in.GetOptions().GetPageToken())
	if len(c.pages) == 0 {
		return &ateapi.ListActorsResponse{}, nil
	}
	resp := c.pages[0]
	c.pages = c.pages[1:]
	return resp, nil
}

// TestActorReferencingTemplateFollowsAllPages covers the template cleanup:
// an actor that references the template only appears on the second page, so a
// single-page lookup would wrongly let the template be deleted.
func TestActorReferencingTemplateFollowsAllPages(t *testing.T) {
	const template = "cw-0123456789ab"
	unrelated := &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Name: "cw-unrelated"},
		ActorTemplate: &ateapi.ObjectRef{Atespace: paginationAtespace, Name: "cw-other"},
	}
	referencing := &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Name: "cw-later"},
		ActorTemplate: &ateapi.ObjectRef{Atespace: paginationAtespace, Name: template},
	}
	client := &pagedActorsClient{pages: []*ateapi.ListActorsResponse{
		{Actors: []*ateapi.Actor{unrelated}, NextPageToken: "page-2"},
		{Actors: []*ateapi.Actor{referencing}},
	}}

	got, err := actorReferencingTemplate(context.Background(), client, paginationAtespace, template)
	if err != nil {
		t.Fatalf("actorReferencingTemplate: %v", err)
	}
	if got != "cw-later" {
		t.Errorf("actorReferencingTemplate = %q, want %q; a reference on the second page was missed", got, "cw-later")
	}
	if len(client.tokens) != 2 || client.tokens[0] != "" || client.tokens[1] != "page-2" {
		t.Errorf("ListActors page tokens = %q, want [\"\" \"page-2\"]", client.tokens)
	}

	empty := &pagedActorsClient{pages: []*ateapi.ListActorsResponse{
		{Actors: []*ateapi.Actor{unrelated}, NextPageToken: "page-2"},
		{Actors: []*ateapi.Actor{unrelated}},
	}}
	got, err = actorReferencingTemplate(context.Background(), empty, paginationAtespace, template)
	if err != nil {
		t.Fatalf("actorReferencingTemplate with no reference: %v", err)
	}
	if got != "" {
		t.Errorf("actorReferencingTemplate with no reference = %q, want an empty name", got)
	}
}
