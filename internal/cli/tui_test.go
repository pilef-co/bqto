package cli

import (
	"cloud.google.com/go/auth"
	"cmp"
	"context"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/pilef-co/bqto/internal/gcpfake"
	"github.com/pilef-co/bqto/quota"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/option"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestFetchAllAgainstTheAPI(t *testing.T) {
	t.Run("every page of projects, and no organization without permission", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			Err("GET", "/v3/organizations:search", http.StatusForbidden, "forbidden", "no resourcemanager.organizations.get").
			OK("GET", "/v3/folders:search?query=state:ACTIVE", map[string]any{"folders": []map[string]any{{"name": "folders/1"}}}).
			OK("GET", "/v3/projects:search?query=state:ACTIVE", map[string]any{
				"projects":      []map[string]any{{"projectId": "a"}},
				"nextPageToken": "page-2",
			}).
			OK("GET", "/v3/projects:search?query=state:ACTIVE&pageToken=page-2", map[string]any{
				"projects": []map[string]any{{"projectId": "b"}},
			})
		orgs, folders, projects, err := fetchAll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(orgs) != 0 || len(folders) != 1 || len(projects) != 2 || projects[1].ProjectId != "b" {
			t.Errorf("fetchAll = %d orgs, %d folders, %d projects", len(orgs), len(folders), len(projects))
		}
	})

	t.Run("projects that cannot be listed return the error", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("GET", "/v3/organizations:search", map[string]any{}).
			OK("GET", "/v3/folders:search?query=state:ACTIVE", map[string]any{}).
			Err("GET", "/v3/projects:search?query=state:ACTIVE", http.StatusForbidden, "forbidden", "no resourcemanager.projects.list")
		if _, _, _, err := fetchAll(context.Background()); err == nil {
			t.Error("err = nil, want the projects error")
		}
	})
}

type adcToken struct{}

func (adcToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "fake", Type: "Bearer"}, nil
}

func TestFetchAllListsLikeGcloudWhenNoQuotaProjectHasTheAPI(t *testing.T) {
	gcpfake.UserADC(t)
	disabled := func(project string) gcpfake.Response {
		return gcpfake.ErrorInfo(http.StatusForbidden, "SERVICE_DISABLED", "Cloud Resource Manager API has not been used in project "+project,
			map[string]string{"consumer": "projects/" + project, "service": "cloudresourcemanager.googleapis.com"})
	}
	fake := gcpfake.Start(t, &clientOptions).
		On("GET", "/v3/organizations:search", disabled("prj-beta"), disabled("prj-beta"), gcpfake.Response{Status: http.StatusOK, Body: map[string]any{}}).
		OK("GET", "/v3/folders:search?query=state:ACTIVE", map[string]any{}).
		OK("GET", "/v3/projects:search?query=state:ACTIVE", map[string]any{"projects": []map[string]any{{"projectId": "a"}, {"projectId": "b"}}})
	clientOptions = append(clientOptions, option.WithAuthCredentials(auth.NewCredentials(&auth.CredentialsOptions{
		TokenProvider: adcToken{},
		QuotaProjectIDProvider: auth.CredentialsPropertyFunc(func(context.Context) (string, error) {
			return "prj-beta", nil
		}),
	})))

	_, _, projects, err := fetchAll(quota.WithProject(context.Background(), "prj-beta"))
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Errorf("fetchAll = %d projects, want 2", len(projects))
	}
	var sent []string
	for _, r := range fake.Requests() {
		sent = append(sent, r.Path+"="+r.UserProject)
	}
	want := "/v3/organizations:search=prj-beta,/v3/organizations:search=prj-beta,/v3/organizations:search=,/v3/folders:search=,/v3/projects:search="
	if got := strings.Join(sent, ","); got != want {
		t.Errorf("calls = %s\nwant    %s", got, want)
	}
}

func TestBuildProjectGroups(t *testing.T) {
	org1 := &crmv3.Organization{Name: "organizations/1", DisplayName: "Acme"}
	org2 := &crmv3.Organization{Name: "organizations/2", DisplayName: "Beta"}

	folderEng := &crmv3.Folder{Name: "folders/10", DisplayName: "Engineering", Parent: "organizations/1"}
	folderData := &crmv3.Folder{Name: "folders/11", DisplayName: "Data", Parent: "folders/10"}

	tests := []struct {
		name     string
		orgs     []*crmv3.Organization
		folders  []*crmv3.Folder
		projects []*crmv3.Project

		wantGroups []struct {
			orgDisplay string
			projects   []string
		}
	}{
		{
			name:    "project directly under org",
			orgs:    []*crmv3.Organization{org1},
			folders: nil,
			projects: []*crmv3.Project{
				{ProjectId: "proj-a", DisplayName: "Proj A", Parent: "organizations/1"},
			},
			wantGroups: []struct {
				orgDisplay string
				projects   []string
			}{
				{"Acme", []string{"proj-a:"}},
			},
		},
		{
			name:    "nested folders path",
			orgs:    []*crmv3.Organization{org1},
			folders: []*crmv3.Folder{folderEng, folderData},
			projects: []*crmv3.Project{
				{ProjectId: "proj-b", DisplayName: "Proj B", Parent: "folders/11"},
			},
			wantGroups: []struct {
				orgDisplay string
				projects   []string
			}{
				{"Acme", []string{"proj-b:Engineering / Data"}},
			},
		},
		{
			name:    "unresolvable folder chain goes to No organization with id shown",
			orgs:    []*crmv3.Organization{org1},
			folders: []*crmv3.Folder{},
			projects: []*crmv3.Project{
				{ProjectId: "proj-c", Parent: "folders/99"},
			},
			wantGroups: []struct {
				orgDisplay string
				projects   []string
			}{
				{"No organization", []string{"proj-c:folders/99"}},
			},
		},
		{
			name:    "project with no parent goes to No organization",
			orgs:    []*crmv3.Organization{org1},
			folders: nil,
			projects: []*crmv3.Project{
				{ProjectId: "proj-d", Parent: ""},
			},
			wantGroups: []struct {
				orgDisplay string
				projects   []string
			}{
				{"No organization", []string{"proj-d:"}},
			},
		},
		{
			name:    "sort order: by folderPath then id within group",
			orgs:    []*crmv3.Organization{org1},
			folders: []*crmv3.Folder{folderEng, folderData},
			projects: []*crmv3.Project{
				{ProjectId: "z-proj", Parent: "organizations/1"},
				{ProjectId: "a-proj", Parent: "organizations/1"},
				{ProjectId: "b-proj", Parent: "folders/11"},
			},
			wantGroups: []struct {
				orgDisplay string
				projects   []string
			}{
				{"Acme", []string{"a-proj:", "z-proj:", "b-proj:Engineering / Data"}},
			},
		},
		{
			name:    "multiple orgs sorted alphabetically, No organization last",
			orgs:    []*crmv3.Organization{org1, org2},
			folders: nil,
			projects: []*crmv3.Project{
				{ProjectId: "p1", Parent: "organizations/2"},
				{ProjectId: "p2", Parent: "organizations/1"},
				{ProjectId: "p3", Parent: ""},
			},
			wantGroups: []struct {
				orgDisplay string
				projects   []string
			}{
				{"Acme", []string{"p2:"}},
				{"Beta", []string{"p1:"}},
				{"No organization", []string{"p3:"}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildProjectGroups(tc.orgs, tc.folders, tc.projects)
			if len(got) != len(tc.wantGroups) {
				t.Fatalf("got %d groups, want %d\n  got: %v", len(got), len(tc.wantGroups), got)
			}
			for i, wg := range tc.wantGroups {
				if got[i].orgDisplay != wg.orgDisplay {
					t.Errorf("group[%d] orgDisplay = %q, want %q", i, got[i].orgDisplay, wg.orgDisplay)
				}
				if len(got[i].projects) != len(wg.projects) {
					t.Fatalf("group[%d] has %d projects, want %d", i, len(got[i].projects), len(wg.projects))
				}
				for j, wantPE := range wg.projects {
					parts := strings.SplitN(wantPE, ":", 2)
					wantID, wantPath := parts[0], parts[1]
					pe := got[i].projects[j]
					if pe.id != wantID || pe.folderPath != wantPath {
						t.Errorf("group[%d].projects[%d] = {id:%q, folderPath:%q}, want {id:%q, folderPath:%q}",
							i, j, pe.id, pe.folderPath, wantID, wantPath)
					}
				}
			}
		})
	}
}

func TestProjectSelectShowsEveryOptionWithoutBlankRows(t *testing.T) {
	long := "A project with a rather long display name (long-project-id) — Engineering / Platform"
	for _, tc := range []struct {
		desc   string
		labels []string
		width  int
		rows   int
		shows  string
	}{
		{"one project", []string{"only (only)"}, 80, 2, ""},
		{"three projects", []string{"a", "b", "c"}, 80, 4, ""},
		{"eleven projects", slices.Repeat([]string{"p"}, 11), 80, 12, ""},
		{"forty projects", slices.Repeat([]string{"p"}, 40), 80, 12, ""},
		{"one long label on a wide terminal", []string{long}, 200, 2, ""},
		{"one long label on a narrow terminal", []string{long}, 30, 5, "Platform"},
		{"long labels capped", slices.Repeat([]string{long}, 5), 30, 12, ""},
	} {
		opts := make([]huh.Option[string], len(tc.labels))
		for i, l := range tc.labels {
			opts[i] = huh.NewOption(l, fmt.Sprintf("p%d", i))
		}
		var chosen string
		view := projectSelect(opts, &chosen, tc.width).WithWidth(tc.width).View()
		if got := strings.Count(view, "\n") + 1; got != tc.rows {
			t.Errorf("%s: the picker is %d rows tall, want %d:\n%s", tc.desc, got, tc.rows, view)
		}
		lines := strings.Split(view, "\n")
		if tc.rows <= maxProjectRows && strings.TrimSpace(lines[len(lines)-1]) == "" {
			t.Errorf("%s: the picker ends in a blank row:\n%s", tc.desc, view)
		}
		if tc.shows != "" && !strings.Contains(view, tc.shows) {
			t.Errorf("%s: the picker cuts the label before %q:\n%s", tc.desc, tc.shows, view)
		}
		if !strings.Contains(view, ">") {
			t.Errorf("%s: the selected project is not shown:\n%s", tc.desc, view)
		}
	}
}

func TestDefaultFirstMovesTheDefaultProjectToTheTop(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
	}{
		{"c", []string{"c", "a", "b", "d"}},
		{"a", []string{"a", "b", "c", "d"}},
		{"b", []string{"b", "a", "c", "d"}},
		{"d", []string{"d", "a", "b", "c"}},
		{"missing", []string{"a", "b", "c", "d"}},
	} {
		opts := huh.NewOptions("a", "b", "c", "d")
		defaultFirst(opts, tc.value)
		var got []string
		for _, o := range opts {
			got = append(got, o.Value)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("defaultFirst(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestPickerWithTheDefaultProjectShowsEveryProject(t *testing.T) {
	opts := huh.NewOptions("alpha", "bravo", "charlie")
	chosen := "charlie"
	defaultFirst(opts, chosen)
	view := projectSelect(opts, &chosen, 80).WithWidth(80).View()
	for _, name := range []string{"alpha", "bravo", "> charlie"} {
		if !strings.Contains(view, name) {
			t.Errorf("the picker hides %q:\n%s", name, view)
		}
	}
}

func render(field huh.Field) string {
	form := huh.NewForm(huh.NewGroup(field))
	form.Init()
	model, _ := form.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	return model.View()
}

func TestOrganizationPickerShowsEveryOrganization(t *testing.T) {
	groups := []orgGroup{
		{orgID: "111", orgDisplay: "example.com", projects: []projectEntry{{id: "work"}}},
		{orgID: "222", orgDisplay: "example.org", projects: []projectEntry{{id: "lab"}, {id: "lab2"}}},
		{orgDisplay: "No organization", projects: []projectEntry{{id: "home"}}},
	}
	for _, tc := range []struct {
		defProject string
		selected   string
	}{
		{"home", "No organization"},
		{"lab2", "example.org (2)"},
		{"work", "example.com (1)"},
		{"", "example.com (1)"},
		{"unknown", "example.com (1)"},
	} {
		var chosen string
		view := render(orgSelect(groups, tc.defProject, &chosen))
		opts := orgOptions(groups, tc.defProject)
		for _, name := range []string{"example.com (1)", "example.org (2)", "No organization", "> " + tc.selected} {
			if !strings.Contains(view, name) {
				t.Errorf("default project %q: the picker hides %q:\n%s", tc.defProject, name, view)
			}
		}
		if want := opts[0].Value; chosen != want {
			t.Errorf("default project %q: the picker starts on %q, want %q", tc.defProject, chosen, want)
		}
	}
}

func TestPickerShowsEveryOptionAndStartsOnTheEmptyValue(t *testing.T) {
	profiles := make([]string, 15)
	for i := range profiles {
		profiles[i] = fmt.Sprintf("profile-%02d", i)
	}
	for _, tc := range []struct {
		desc   string
		values []string
		want   string
	}{
		{"new connection last", []string{"a", "b", ""}, ""},
		{"new connection after many profiles", append(slices.Clone(profiles), ""), ""},
		{"empty value in the middle", []string{"a", "", "b"}, ""},
		{"empty value first", []string{"", "a"}, ""},
		{"no empty value", []string{"a", "b", "c"}, "a"},
		{"one option", []string{"a"}, "a"},
	} {
		opts := make([]huh.Option[string], len(tc.values))
		for i, v := range tc.values {
			opts[i] = huh.NewOption("option-"+cmp.Or(v, "new"), v)
		}
		var chosen string
		view := render(pickerSelect("Pick", opts, &chosen))
		for _, o := range opts {
			want := o.Key
			if o.Value == tc.want {
				want = "> " + want
			}
			if !strings.Contains(view, want) {
				t.Errorf("%s: the picker hides %q:\n%s", tc.desc, want, view)
			}
		}
		if chosen != tc.want {
			t.Errorf("%s: the picker starts on %q, want %q", tc.desc, chosen, tc.want)
		}
	}
}

func ancestry(ids ...string) map[string]any {
	var ancestors []map[string]any
	for _, id := range ids {
		kind, num, _ := strings.Cut(id, "/")
		ancestors = append(ancestors, map[string]any{"resourceId": map[string]any{"type": kind, "id": num}})
	}
	return map[string]any{"ancestor": ancestors}
}

func TestFetchAllFindsTheOrganizationOfAFolderItCannotList(t *testing.T) {
	fake := gcpfake.Start(t, &clientOptions).
		OK("GET", "/v3/organizations:search", map[string]any{"organizations": []map[string]any{{"name": "organizations/1", "displayName": "example.com"}}}).
		OK("GET", "/v3/folders:search?query=state:ACTIVE", map[string]any{"folders": []map[string]any{{"name": "folders/8", "displayName": "Team", "parent": "organizations/1"}}}).
		OK("GET", "/v3/projects:search?query=state:ACTIVE", map[string]any{"projects": []map[string]any{
			{"projectId": "prj-sandbox", "parent": "folders/300"},
			{"projectId": "prj-other", "parent": "folders/300"},
			{"projectId": "prj-nested", "parent": "folders/7"},
			{"projectId": "prj-denied", "parent": "folders/9"},
			{"projectId": "prj-denied-too", "parent": "folders/9"},
			{"projectId": "prj-deep", "parent": "folders/71"},
			{"projectId": "prj-mid", "parent": "folders/72"},
			{"projectId": "prj-org", "parent": "organizations/1"},
			{"projectId": "prj-alone"},
		}}).
		OK("POST", "/v1/projects/prj-sandbox:getAncestry", ancestry("project/prj-sandbox", "folder/300", "organization/1")).
		OK("POST", "/v1/projects/prj-nested:getAncestry", ancestry("project/prj-nested", "folder/7", "folder/8", "organization/1")).
		OK("POST", "/v1/projects/prj-deep:getAncestry", ancestry("project/prj-deep", "folder/71", "folder/72", "organization/1")).
		Err("POST", "/v1/projects/prj-denied:getAncestry", http.StatusForbidden, "forbidden", "no resourcemanager.projects.get")
	orgs, folders, projects, err := fetchAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	groups := buildProjectGroups(orgs, folders, projects)
	var got []string
	for _, g := range groups {
		for _, pe := range g.projects {
			got = append(got, g.orgDisplay+": "+pe.id+" — "+pe.folderPath)
		}
	}
	want := []string{
		"example.com: prj-org — ",
		"example.com: prj-nested — Team / folders/7",
		"example.com: prj-other — folders/300",
		"example.com: prj-sandbox — folders/300",
		"example.com: prj-mid — folders/72",
		"example.com: prj-deep — folders/72 / folders/71",
		"No organization: prj-alone — ",
		"No organization: prj-denied — folders/9",
		"No organization: prj-denied-too — folders/9",
	}
	if n := len(folders); n != 5 {
		t.Errorf("fetchAll = %d folders, want 5 with no duplicate", n)
	}
	if !slices.Equal(got, want) {
		t.Errorf("groups =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	ancestryCalls := 0
	for _, r := range fake.Requests() {
		if strings.HasSuffix(r.Path, ":getAncestry") {
			ancestryCalls++
		}
	}
	if ancestryCalls != 4 {
		t.Errorf("fetchAll asked for %d ancestries, want 4, one per unknown folder", ancestryCalls)
	}
}

func TestFillFolderGapsStopsAfterTheLimit(t *testing.T) {
	fake := gcpfake.Start(t, &clientOptions)
	var projects []*crmv3.Project
	for i := range maxAncestryCalls + 3 {
		id := fmt.Sprintf("p%d", i)
		projects = append(projects, &crmv3.Project{ProjectId: id, Parent: fmt.Sprintf("folders/%d", i)})
		if i < maxAncestryCalls {
			fake.Err("POST", "/v1/projects/"+id+":getAncestry", http.StatusForbidden, "forbidden", "denied")
		}
	}
	if got := fillFolderGaps(context.Background(), nil, projects); len(got) != 0 {
		t.Errorf("fillFolderGaps = %d folders, want none", len(got))
	}
	if n := len(fake.Requests()); n != maxAncestryCalls {
		t.Errorf("fillFolderGaps made %d calls, want %d", n, maxAncestryCalls)
	}
}

func TestUnknownFolder(t *testing.T) {
	known := map[string]*crmv3.Folder{
		"folders/1": {Name: "folders/1", Parent: "organizations/9"},
		"folders/2": {Name: "folders/2", Parent: "folders/3"},
		"folders/4": {Name: "folders/4", Parent: "folders/5"},
		"folders/5": {Name: "folders/5", Parent: "folders/4"},
	}
	for _, tc := range []struct{ parent, want string }{
		{"", ""},
		{"organizations/9", ""},
		{"folders/1", ""},
		{"folders/2", "folders/3"},
		{"folders/6", "folders/6"},
		{"folders/4", ""},
	} {
		if got := unknownFolder(tc.parent, known); got != tc.want {
			t.Errorf("unknownFolder(%q) = %q, want %q", tc.parent, got, tc.want)
		}
	}
}

func TestFoldersFromAncestry(t *testing.T) {
	for _, tc := range []struct {
		desc string
		ids  []string
		want []string
	}{
		{"one folder in an organization", []string{"project/p", "folder/1", "organization/9"}, []string{"folders/1 < organizations/9"}},
		{"nested folders", []string{"project/p", "folder/1", "folder/2", "organization/9"}, []string{"folders/1 < folders/2", "folders/2 < organizations/9"}},
		{"no folder", []string{"project/p", "organization/9"}, nil},
		{"a folder with no parent listed", []string{"project/p", "folder/1"}, nil},
		{"an unknown parent type", []string{"project/p", "folder/1", "galaxy/1"}, nil},
	} {
		var ancestors []*crmv1.Ancestor
		for _, id := range tc.ids {
			kind, num, _ := strings.Cut(id, "/")
			ancestors = append(ancestors, &crmv1.Ancestor{ResourceId: &crmv1.ResourceId{Type: kind, Id: num}})
		}
		var got []string
		for _, f := range foldersFromAncestry(ancestors) {
			got = append(got, f.Name+" < "+f.Parent)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: folders = %q, want %q", tc.desc, got, tc.want)
		}
	}
	if got := foldersFromAncestry([]*crmv1.Ancestor{{}, {ResourceId: &crmv1.ResourceId{Type: "organization", Id: "9"}}}); len(got) != 0 {
		t.Errorf("an ancestor without a resource id gives %v", got)
	}
}

func TestTheProjectListWarningSuggestsAnotherQuotaProject(t *testing.T) {
	disabled := errors.New("list projects: Cloud Resource Manager API is disabled\nFix: gcloud services enable cloudresourcemanager.googleapis.com --project beta")
	expired := errors.New("list projects: your login expired\nFix: gcloud auth application-default login")
	tests := []struct {
		err       error
		canSwitch bool
		want      string
	}{
		{disabled, true, "Cloud Resource Manager API is disabled\nFix: " + switchQuotaFix},
		{disabled, false, "Cloud Resource Manager API is disabled\nFix: gcloud services enable cloudresourcemanager.googleapis.com --project beta"},
		{expired, true, "your login expired\nFix: gcloud auth application-default login"},
		{errors.New("list projects: no credentials"), true, "no credentials"},
	}
	for _, tc := range tests {
		want := "warn: cannot list your projects, so type one below: " + tc.want
		if got := listProjectsWarning(tc.err, tc.canSwitch); got != want {
			t.Errorf("listProjectsWarning(%q, %v) = %q; want %q", tc.err, tc.canSwitch, got, want)
		}
	}
}
