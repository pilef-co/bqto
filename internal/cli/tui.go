package cli

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/cellbuf"
	"github.com/charmbracelet/x/term"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"
	"github.com/pilef-co/bqto/internal/auth"
	"github.com/pilef-co/bqto/quota"
	"google.golang.org/api/option"
)

var clientOptions []option.ClientOption

type projectEntry struct {
	id         string
	display    string
	folderPath string
}

type orgGroup struct {
	orgID      string
	orgDisplay string
	projects   []projectEntry
}

func buildProjectGroups(orgs []*crmv3.Organization, folders []*crmv3.Folder, projects []*crmv3.Project) []orgGroup {
	orgByName := make(map[string]*crmv3.Organization, len(orgs))
	for _, o := range orgs {
		orgByName[o.Name] = o
	}
	folderByName := make(map[string]*crmv3.Folder, len(folders))
	for _, f := range folders {
		folderByName[f.Name] = f
	}

	byOrg := make(map[string][]projectEntry)
	for _, p := range projects {
		orgID, folderPath := resolveProjectParent(p.Parent, folderByName)
		byOrg[orgID] = append(byOrg[orgID], projectEntry{id: p.ProjectId, display: p.DisplayName, folderPath: folderPath})
	}

	var groups []orgGroup
	for orgID, entries := range byOrg {
		if orgID == "" {
			continue
		}
		display := orgID
		if o, ok := orgByName[orgID]; ok && o.DisplayName != "" {
			display = o.DisplayName
		}
		sortProjectEntries(entries)
		groups = append(groups, orgGroup{orgID: orgID, orgDisplay: display, projects: entries})
	}
	slices.SortFunc(groups, func(a, b orgGroup) int { return cmp.Compare(a.orgDisplay, b.orgDisplay) })
	if entries, ok := byOrg[""]; ok {
		sortProjectEntries(entries)
		groups = append(groups, orgGroup{orgID: "", orgDisplay: "No organization", projects: entries})
	}
	return groups
}

func resolveProjectParent(parent string, folderByName map[string]*crmv3.Folder) (orgID, folderPath string) {

	var parts []string
	cur := parent
	for strings.HasPrefix(cur, "folders/") {
		f, ok := folderByName[cur]
		if !ok {
			parts = append(parts, cur)
			break
		}
		parts = append(parts, cmp.Or(f.DisplayName, f.Name))
		cur = f.Parent
	}
	slices.Reverse(parts)
	if strings.HasPrefix(cur, "organizations/") {
		orgID = cur
	}
	return orgID, strings.Join(parts, " / ")
}

func sortProjectEntries(entries []projectEntry) {
	slices.SortFunc(entries, func(a, b projectEntry) int {
		return cmp.Or(cmp.Compare(a.folderPath, b.folderPath), cmp.Compare(a.id, b.id))
	})
}

func fetchAll(ctx context.Context) ([]*crmv3.Organization, []*crmv3.Folder, []*crmv3.Project, error) {
	svc, err := crmv3.NewService(ctx, quota.ListingOptions(ctx, clientOptions...)...)
	if err != nil {
		return nil, nil, nil, gcperr.Wrap("connect to the Resource Manager API", err)
	}

	orgs, err := gcperr.Retry(ctx, func() ([]*crmv3.Organization, error) {
		var orgs []*crmv3.Organization
		err := svc.Organizations.Search().Pages(ctx, func(p *crmv3.SearchOrganizationsResponse) error {
			orgs = append(orgs, p.Organizations...)
			return nil
		})
		return orgs, err
	})
	var skipped []string
	if err != nil {
		skipped = append(skipped, "list organizations: "+gcperr.Message(err))
	}

	folders, err := gcperr.Retry(ctx, func() ([]*crmv3.Folder, error) {
		var folders []*crmv3.Folder
		err := svc.Folders.Search().Query("state:ACTIVE").Pages(ctx, func(p *crmv3.SearchFoldersResponse) error {
			folders = append(folders, p.Folders...)
			return nil
		})
		return folders, err
	})
	if err != nil {
		skipped = append(skipped, "list folders: "+gcperr.Message(err))
	}

	projects, err := gcperr.Retry(ctx, func() ([]*crmv3.Project, error) {
		var projects []*crmv3.Project
		err := svc.Projects.Search().Query("state:ACTIVE").Pages(ctx, func(p *crmv3.SearchProjectsResponse) error {
			projects = append(projects, p.Projects...)
			return nil
		})
		return projects, err
	})
	if err != nil {
		for _, w := range skipped {
			ext.Debugf("%s", w)
		}
		return nil, nil, nil, gcperr.Wrap("list projects", err)
	}
	for _, w := range skipped {
		ext.Warnf("%s", w)
	}

	return orgs, fillFolderGaps(ctx, folders, projects), projects, nil
}

const maxAncestryCalls = 10

func fillFolderGaps(ctx context.Context, folders []*crmv3.Folder, projects []*crmv3.Project) []*crmv3.Folder {
	folderByName := make(map[string]*crmv3.Folder, len(folders))
	for _, f := range folders {
		folderByName[f.Name] = f
	}
	var svc *crmv1.Service
	asked := map[string]bool{}
	for _, p := range projects {
		gap := unknownFolder(p.Parent, folderByName)
		if gap == "" || asked[gap] || len(asked) >= maxAncestryCalls {
			continue
		}
		asked[gap] = true
		if svc == nil {
			var err error
			if svc, err = crmv1.NewService(ctx, quota.ListingOptions(ctx, clientOptions...)...); err != nil {
				ext.Debugf("connect to the Resource Manager v1 API: %s", gcperr.Message(err))
				return folders
			}
		}
		resp, err := gcperr.Retry(ctx, func() (*crmv1.GetAncestryResponse, error) {
			return svc.Projects.GetAncestry(p.ProjectId, &crmv1.GetAncestryRequest{}).Context(ctx).Do()
		})
		if err != nil {
			ext.Debugf("ancestry of project %s: %s", p.ProjectId, gcperr.Message(err))
			continue
		}
		for _, f := range foldersFromAncestry(resp.Ancestor) {
			if _, ok := folderByName[f.Name]; !ok {
				folderByName[f.Name] = f
				folders = append(folders, f)
			}
		}
	}
	return folders
}

func unknownFolder(parent string, folderByName map[string]*crmv3.Folder) string {
	cur := parent
	for range len(folderByName) + 1 {
		if !strings.HasPrefix(cur, "folders/") {
			return ""
		}
		f, ok := folderByName[cur]
		if !ok {
			return cur
		}
		cur = f.Parent
	}
	return ""
}

func foldersFromAncestry(ancestors []*crmv1.Ancestor) []*crmv3.Folder {
	var folders []*crmv3.Folder
	for i := 0; i+1 < len(ancestors); i++ {
		self, parent := ancestors[i].ResourceId, ancestors[i+1].ResourceId
		if self == nil || parent == nil || self.Type != "folder" || (parent.Type != "folder" && parent.Type != "organization") {
			continue
		}
		folders = append(folders, &crmv3.Folder{Name: "folders/" + self.Id, Parent: parent.Type + "s/" + parent.Id})
	}
	return folders
}

func pickOne(ctx context.Context, title string, opts []huh.Option[string]) (string, error) {
	var chosen string
	err := huh.NewForm(huh.NewGroup(
		pickerSelect(title, opts, &chosen),
	)).RunWithContext(ctx)
	return chosen, err
}

func orgSelect(groups []orgGroup, defProject string, chosen *string) *huh.Select[string] {
	opts := orgOptions(groups, defProject)
	*chosen = opts[0].Value
	return pickerSelect("Organization", opts, chosen)
}

func pickerSelect(title string, opts []huh.Option[string], chosen *string) *huh.Select[string] {
	var top string
	if len(opts) > 0 {
		top = opts[0].Value
	}
	return huh.NewSelect[string]().Title(title).Value(&top).Options(opts...).Value(chosen)
}

const (
	maxProjectRows     = 11
	selectFrameWidth   = 4
	fallbackTermWidth  = 80
	huhWrapBreakpoints = ",.-; "
)

func projectSelect(opts []huh.Option[string], value *string, width int) *huh.Select[string] {
	return huh.NewSelect[string]().
		Title("GCP Project").
		Options(opts...).
		Value(value).
		Filtering(true).
		Height(projectRows(opts, width) + 1)
}

func projectRows(opts []huh.Option[string], width int) int {
	rows := 0
	for _, o := range opts {
		rows += strings.Count(cellbuf.Wrap(o.Key, width-selectFrameWidth, huhWrapBreakpoints), "\n") + 1
		if rows >= maxProjectRows {
			return maxProjectRows
		}
	}
	return rows
}

func formWidth() int {
	if w, _, err := term.GetSize(os.Stderr.Fd()); err == nil && w > selectFrameWidth {
		return w
	}
	return fallbackTermWidth
}

func defaultFirst(opts []huh.Option[string], value string) {
	i := slices.IndexFunc(opts, func(o huh.Option[string]) bool { return o.Value == value })
	if i > 0 {
		chosen := opts[i]
		copy(opts[1:i+1], opts[:i])
		opts[0] = chosen
	}
}

func orgOptions(groups []orgGroup, defProject string) []huh.Option[string] {
	opts := make([]huh.Option[string], len(groups))
	for i, g := range groups {
		label := g.orgDisplay
		if g.orgID != "" {
			label = fmt.Sprintf("%s (%d)", g.orgDisplay, len(g.projects))
		}
		opts[i] = huh.NewOption(label, g.orgID)
	}
	defaultFirst(opts, groupOf(groups, defProject))
	return opts
}

func groupOf(groups []orgGroup, project string) string {
	for _, g := range groups {
		if slices.ContainsFunc(g.projects, func(pe projectEntry) bool { return pe.id == project }) {
			return g.orgID
		}
	}
	return groups[0].orgID
}

const switchQuotaFix = "pick a quota project where the Cloud Resource Manager API is on: gcloud auth application-default set-quota-project PROJECT"

func listProjectsWarning(err error, canSwitchQuota bool) string {
	msg, fix := gcperr.SplitFix(err)
	msg = strings.TrimPrefix(msg, "list projects: ")
	if canSwitchQuota && strings.HasPrefix(fix, "gcloud services enable cloudresourcemanager.googleapis.com") {
		fix = switchQuotaFix
	}
	if fix == "" {
		return "warn: cannot list your projects, so type one below: " + msg
	}
	return "warn: cannot list your projects, so type one below: " + msg + "\nFix: " + fix
}

func runWizard(ctx context.Context, prefill connectConfig) (connectConfig, error) {

	cfg := connectConfig{Project: prefill.Project, Instance: prefill.Instance, Service: cmp.Or(prefill.Service, "cloudsql"), Duration: "30m", Options: maps.Clone(prefill.Options)}

	var step1Fields []huh.Field
	if cfg.Project == "" {
		defProject := auth.DefaultProject()
		ext.StartProgress("loading organizations and projects… ")
		orgs, folders, rawProjects, fetchErr := fetchAll(quota.WithProject(ctx, defProject))
		ext.EndProgress("")

		var projectField huh.Field
		if fetchErr != nil || len(rawProjects) == 0 {
			if fetchErr != nil {
				fmt.Fprintln(os.Stderr, listProjectsWarning(fetchErr, os.Getenv(quota.EnvVar) == "" && quota.UserCredentials()))
			}
			cfg.Project = defProject
			projectField = huh.NewInput().
				Title("GCP Project").
				Placeholder(cfg.Project).
				Value(&cfg.Project)
		} else {
			groups := buildProjectGroups(orgs, folders, rawProjects)

			targetGroup := groups[0]
			if len(groups) > 1 {
				var chosenOrgID string
				if err := huh.NewForm(huh.NewGroup(orgSelect(groups, defProject, &chosenOrgID))).RunWithContext(ctx); err != nil {
					return connectConfig{}, err
				}
				if i := slices.IndexFunc(groups, func(g orgGroup) bool { return g.orgID == chosenOrgID }); i >= 0 {
					targetGroup = groups[i]
				}
			}

			cfg.Project = defProject
			projectOpts := make([]huh.Option[string], len(targetGroup.projects))
			for i, pe := range targetGroup.projects {
				label := pe.id
				if pe.display != "" && pe.display != pe.id {
					label = pe.display + " (" + pe.id + ")"
				}
				if pe.folderPath != "" {
					label += " — " + pe.folderPath
				}
				projectOpts[i] = huh.NewOption(label, pe.id)
			}
			defaultFirst(projectOpts, cfg.Project)
			projectField = projectSelect(projectOpts, &cfg.Project, formWidth())
		}
		step1Fields = append(step1Fields, projectField)
	}
	if prefill.Service == "" {
		var serviceOpts []huh.Option[string]
		for _, svc := range ext.Services() {
			serviceOpts = append(serviceOpts, huh.NewOption(svc.Title, svc.Name))
		}
		step1Fields = append(step1Fields,
			huh.NewSelect[string]().
				Title("Service").
				Options(serviceOpts...).
				Value(&cfg.Service),
		)
	}

	if len(step1Fields) > 0 {
		form1 := huh.NewForm(huh.NewGroup(step1Fields...))
		if err := form1.RunWithContext(ctx); err != nil {
			return connectConfig{}, err
		}
	}

	svc, ok := ext.LookupService(cfg.Service)
	if !ok {
		return connectConfig{}, unsupportedService(cfg.Service)
	}
	ext.StartProgress("loading " + svc.Noun + "… ")
	instances, err := instanceOptions(ctx, svc, cfg.Project)
	if err != nil {
		ext.EndProgress("")
		return connectConfig{}, err
	}
	ext.EndProgress("done")
	if len(instances) == 0 {
		return connectConfig{}, fmt.Errorf("no %s %s found in project %s\nFix: pick another project, or pass --project", svc.Title, svc.Noun, cfg.Project)
	}

	step2Fields := []huh.Field{
		huh.NewSelect[string]().
			Title(svc.InstanceTitle).
			Options(instances...).
			Value(&cfg.Instance),
		huh.NewSelect[string]().
			Title("Session duration").
			Options(
				huh.NewOption("30 minutes", "30m"),
				huh.NewOption("1 hour", "1h"),
				huh.NewOption("2 hours", "2h"),
			).
			Value(&cfg.Duration),
	}
	extra := toExt(cfg)
	var commits []func()
	for _, h := range ext.Connect() {
		if h.Wizard == nil {
			continue
		}
		for _, q := range h.Wizard(&extra) {
			if q.Commit != nil {
				commits = append(commits, q.Commit)
			}
			switch {
			case q.Bool != nil:
				step2Fields = append(step2Fields, huh.NewConfirm().Title(q.Title).Affirmative("Yes").Negative("No").Value(q.Bool))
			case q.Text != nil:
				step2Fields = append(step2Fields, huh.NewInput().Title(q.Title).Value(q.Text))
			}
		}
	}
	form2 := huh.NewForm(huh.NewGroup(step2Fields...))
	if err := form2.RunWithContext(ctx); err != nil {
		return connectConfig{}, err
	}
	for _, commit := range commits {
		commit()
	}
	answered := fromExt(extra)
	answered.Project, answered.Service, answered.Instance, answered.Duration = cfg.Project, cfg.Service, cfg.Instance, cfg.Duration
	return answered, nil
}

func instanceOptions(ctx context.Context, svc ext.Service, project string) ([]huh.Option[string], error) {
	ctx = quota.WithProject(ctx, project)
	lists := []func(context.Context, string) ([]ext.Choice, error){svc.List}
	for _, v := range ext.VariantsOf(svc.Name) {
		lists = append(lists, v.List)
	}
	var opts []huh.Option[string]
	for _, list := range lists {
		if list == nil {
			continue
		}
		choices, err := list(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, c := range choices {
			opts = append(opts, huh.NewOption(c.Label, c.Value))
		}
	}
	return opts, nil
}
