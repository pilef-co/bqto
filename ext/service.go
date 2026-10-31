package ext

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
)

type Choice struct {
	Label string
	Value string
}

type Forward struct {
	Host string
	Port int
}

type Request struct {
	Project     string
	Instance    string
	Network     string
	Region      string
	Interactive bool
	Explicit    map[string]bool
	Options     map[string]string
	Resolved    string
}

type Target struct {
	Instance                 string
	NetworkProject           string
	Network                  string
	Region                   string
	Subnet                   string
	Forward                  *Forward
	Summary                  string
	Remote                   string
	TLS                      bool
	Auth                     string
	NeedsPrivateGoogleAccess bool
	JSON                     map[string]any
	Hints                    func(Ready) []string
	Env                      func(localPort int) []string
	Start                    func(ctx context.Context, s Ready) (Started, error)
}

type Ready struct {
	Index        int
	LocalPort    int
	Account      string
	Impersonated string
	Options      map[string]string
	Started      Started
	FreePort     func(want int) (int, error)
	ReservePort  func(want int) (port int, release func(), err error)
}

type Started struct {
	Ports map[string]int
	Env   []string
	JSON  map[string]any
	After []string
	Done  <-chan error
	Close func()
}

type Host struct {
	Progress      func(msg string)
	Warn          func(msg string)
	Interactive   bool
	Pick          func(ctx context.Context, title string, options []string) (string, error)
	SubnetNetwork func(ctx context.Context, subnet string) (project, network string, err error)
	Networks      func(ctx context.Context, project string) ([]string, error)
	PickNetwork   func(serviceProject, serviceNetwork, flagNetwork, project string, networks []string) (netProject, network string, err error)
	NetworkFor    func(flagNetwork, project, serviceProject, serviceNetwork string) (netProject, network string, err error)
}

type Service struct {
	Name          string
	Title         string
	Noun          string
	InstanceTitle string
	RefHelp       string
	Order         int
	List          func(ctx context.Context, project string) ([]Choice, error)
	Resolve       func(ctx context.Context, h Host, r Request) (Target, error)
}

type Variant struct {
	Feature string
	Service string
	List    func(ctx context.Context, project string) ([]Choice, error)
	Resolve func(ctx context.Context, h Host, r Request) (Target, error)
}

type NeedsFeature struct {
	Feature string
	Ref     string
	Name    string
}

func (e NeedsFeature) Error() string {
	return fmt.Sprintf("%s needs %s", e.Ref, e.Feature)
}

var (
	mu       sync.RWMutex
	services = map[string]Service{}
	variants = map[string]Variant{}
)

func RegisterService(s Service) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := services[s.Name]; dup {
		panic("ext: service " + s.Name + " registered twice")
	}
	services[s.Name] = s
}

func LookupService(name string) (Service, bool) {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := services[name]
	return s, ok
}

func Services() []Service {
	mu.RLock()
	defer mu.RUnlock()
	all := slices.Collect(maps.Values(services))
	slices.SortFunc(all, func(a, b Service) int { return cmp.Or(cmp.Compare(a.Order, b.Order), cmp.Compare(a.Name, b.Name)) })
	return all
}

func RegisterVariant(v Variant) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := variants[v.Feature]; dup {
		panic("ext: variant " + v.Feature + " registered twice")
	}
	variants[v.Feature] = v
}

func LookupVariant(feature string) (Variant, bool) {
	mu.RLock()
	defer mu.RUnlock()
	v, ok := variants[feature]
	return v, ok
}

func VariantsOf(service string) []Variant {
	mu.RLock()
	defer mu.RUnlock()
	var out []Variant
	for _, v := range variants {
		if v.Service == service {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b Variant) int { return cmp.Compare(a.Feature, b.Feature) })
	return out
}
