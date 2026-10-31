package cli

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pilef-co/bqto/quota"
)

func TestExpandSubnet(t *testing.T) {
	tests := []struct {
		subnet, project, region, want string
	}{
		{
			subnet:  "default",
			project: "my-proj",
			region:  "us-central1",
			want:    "projects/my-proj/regions/us-central1/subnetworks/default",
		},
		{

			subnet:  "projects/other/regions/us-east1/subnetworks/custom",
			project: "my-proj",
			region:  "us-central1",
			want:    "projects/other/regions/us-east1/subnetworks/custom",
		},
		{
			subnet:  "my-subnet",
			project: "net-proj",
			region:  "europe-west1",
			want:    "projects/net-proj/regions/europe-west1/subnetworks/my-subnet",
		},
	}
	for _, tc := range tests {
		got := expandSubnet(tc.subnet, tc.project, tc.region)
		if got != tc.want {
			t.Errorf("expandSubnet(%q, %q, %q) = %q, want %q",
				tc.subnet, tc.project, tc.region, got, tc.want)
		}
	}
}

func TestDefaultPort(t *testing.T) {
	tests := []struct {
		socks  bool
		remote int
		want   int
	}{
		{false, 5432, 15432},
		{false, 3306, 13306},
		{true, 0, 1080},
		{true, 5432, 1080},
		{true, 9092, 1080},
	}
	for _, tc := range tests {
		if got := defaultPort(tc.socks, tc.remote); got != tc.want {
			t.Errorf("defaultPort(%v, %d) = %d, want %d", tc.socks, tc.remote, got, tc.want)
		}
	}
}

func TestPickNetwork(t *testing.T) {
	tests := []struct {
		name            string
		svcNetProj      string
		svcNet          string
		flagNetwork     string
		project         string
		networks        []string
		wantNetProj     string
		wantNet         string
		wantErrContains string
	}{
		{
			name:        "service network wins",
			svcNetProj:  "svc-proj",
			svcNet:      "svc-net",
			project:     "cfg-proj",
			wantNetProj: "svc-proj",
			wantNet:     "svc-net",
		},
		{
			name:        "service network with empty svcNetProj falls back to project",
			svcNetProj:  "",
			svcNet:      "svc-net",
			project:     "cfg-proj",
			wantNetProj: "cfg-proj",
			wantNet:     "svc-net",
		},
		{
			name:        "flag network wins when no service network",
			flagNetwork: "my-net",
			project:     "p",
			wantNetProj: "p",
			wantNet:     "my-net",
		},
		{
			name:        "single network fallback",
			project:     "p",
			networks:    []string{"default"},
			wantNetProj: "p",
			wantNet:     "default",
		},
		{
			name:            "ambiguous: multiple networks",
			project:         "p",
			networks:        []string{"net-a", "net-b"},
			wantErrContains: "--network",
		},
		{
			name:            "ambiguous: no networks found",
			project:         "p",
			networks:        []string{},
			wantErrContains: "--network",
		},
		{
			name:        "full resource name flag parses project and network",
			flagNetwork: "projects/net-proj/global/networks/my-net",
			project:     "cfg-proj",
			wantNetProj: "net-proj",
			wantNet:     "my-net",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotProj, gotNet, err := pickNetwork(tc.svcNetProj, tc.svcNet, tc.flagNetwork, tc.project, tc.networks)
			if tc.wantErrContains != "" {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Errorf("error = %q, want to contain %q", err.Error(), tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotProj != tc.wantNetProj || gotNet != tc.wantNet {
				t.Errorf("pickNetwork = (%q, %q), want (%q, %q)", gotProj, gotNet, tc.wantNetProj, tc.wantNet)
			}
		})
	}
}

func TestParseNetworkRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		desc        string
		ref         string
		wantProject string
		wantNetwork string
		wantErr     bool
	}{
		{desc: "short name", ref: "shared-vpc", wantProject: "my-project", wantNetwork: "shared-vpc"},
		{desc: "full path", ref: "projects/host-p/global/networks/shared-vpc", wantProject: "host-p", wantNetwork: "shared-vpc"},
		{desc: "empty", wantProject: "my-project"},
		{desc: "half a path", ref: "projects/host-p", wantErr: true},
		{desc: "regional path", ref: "projects/host-p/regions/us-central1/subnetworks/s", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			project, network, err := parseNetworkRef(tc.ref, "my-project")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (project != tc.wantProject || network != tc.wantNetwork) {
				t.Errorf("parseNetworkRef(%q) = %q, %q; want %q, %q", tc.ref, project, network, tc.wantProject, tc.wantNetwork)
			}
		})
	}
}

func TestNetworkFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		desc                          string
		flag, serviceProj, serviceNet string
		wantProject, wantNetwork      string
		wantErr                       bool
	}{
		{desc: "service network", serviceProj: "host-p", serviceNet: "vpc", wantProject: "host-p", wantNetwork: "vpc"},
		{desc: "service network without a project", serviceNet: "vpc", wantProject: "my-project", wantNetwork: "vpc"},
		{desc: "flag beats the service network", flag: "other", serviceProj: "host-p", serviceNet: "vpc", wantProject: "my-project", wantNetwork: "other"},
		{desc: "flag with a full path", flag: "projects/net-p/global/networks/other", serviceNet: "vpc", wantProject: "net-p", wantNetwork: "other"},
		{desc: "bad flag", flag: "projects/net-p/global", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			project, network, err := networkFor(tc.flag, "my-project", tc.serviceProj, tc.serviceNet)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (project != tc.wantProject || network != tc.wantNetwork) {
				t.Errorf("networkFor() = %q, %q; want %q, %q", project, network, tc.wantProject, tc.wantNetwork)
			}
		})
	}
}

func TestSessionNetwork(t *testing.T) {
	t.Parallel()
	db := target{service: "cloudsql", instanceRef: "sql", netProject: "p", network: "vpc", region: "us-central1"}
	cache := target{service: "redis", instanceRef: "cache", netProject: "p", network: "vpc", region: "us-central1"}
	far := target{service: "redis", instanceRef: "cache", netProject: "p", network: "vpc", region: "europe-west1"}
	other := target{service: "kafka", instanceRef: "events", netProject: "p", network: "other", region: "us-central1"}

	np, nw, region, warnings, err := sessionNetwork([]target{db, cache})
	if err != nil || np != "p" || nw != "vpc" || region != "us-central1" || len(warnings) != 0 {
		t.Errorf("same network: got %s/%s %s %v %v", np, nw, region, warnings, err)
	}

	_, _, _, warnings, err = sessionNetwork([]target{db, far})
	if err != nil {
		t.Errorf("another region must not be an error: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "europe-west1") {
		t.Errorf("warnings = %v, want one naming the region", warnings)
	}

	_, _, _, _, err = sessionNetwork([]target{db, other})
	if err == nil || !strings.HasPrefix(err.Error(), "cloudsql ") || !strings.Contains(err.Error(), " are on different VPCs (") {
		t.Errorf("two networks: err = %v, want them refused as different VPCs", err)
	}
}

type fakeListener struct {
	port   int
	closed chan struct{}
	once   sync.Once
}

func (l *fakeListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: l.port} }

type fakePorts struct {
	mu    sync.Mutex
	taken map[int]bool
	held  map[int]*fakeListener
}

func (f *fakePorts) listen(p int) (net.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held == nil {
		f.held = map[int]*fakeListener{}
	}
	if f.taken[p] {
		return nil, errors.New("address already in use")
	}
	if ln, ok := f.held[p]; ok {
		select {
		case <-ln.closed:
		default:
			return nil, errors.New("address already in use")
		}
	}
	ln := &fakeListener{port: p, closed: make(chan struct{})}
	f.held[p] = ln
	return ln, nil
}

func (f *fakePorts) open() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var open []int
	for p, ln := range f.held {
		select {
		case <-ln.closed:
		default:
			open = append(open, p)
		}
	}
	slices.Sort(open)
	return open
}

func TestHoldPorts(t *testing.T) {
	t.Parallel()
	free := &fakePorts{}
	_, got, err := holdPorts([]int{15432, 16379, 1080}, make([]bool, 3), free.listen)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{15432, 16379, 1080}; !slices.Equal(got, want) {
		t.Errorf("ports = %v, want %v", got, want)
	}

	_, got, err = holdPorts([]int{16379, 16379, 16379}, make([]bool, 3), (&fakePorts{}).listen)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{16379, 16380, 16381}; !slices.Equal(got, want) {
		t.Errorf("three targets on one default port got %v, want %v", got, want)
	}

	busy := &fakePorts{taken: map[int]bool{15432: true, 15433: true}}
	_, got, err = holdPorts([]int{15432, 15432}, make([]bool, 2), busy.listen)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{15434, 15435}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if open := busy.open(); !slices.Equal(open, []int{15434, 15435}) {
		t.Errorf("ports held = %v, want the two picked ones held until the session ends", open)
	}
}

func TestHoldPortsKeepsAnAskedForPort(t *testing.T) {
	t.Parallel()
	busy := func() *fakePorts { return &fakePorts{taken: map[int]bool{15432: true}} }
	if _, _, err := holdPorts([]int{15432}, []bool{true}, busy().listen); err == nil || !strings.Contains(err.Error(), "local port 15432 is taken") {
		t.Errorf("a taken asked-for port: err = %v", err)
	}
	collide := busy()
	if _, _, err := holdPorts([]int{16379, 16379}, []bool{true, true}, collide.listen); err == nil || !strings.Contains(err.Error(), "16379") {
		t.Errorf("two asked-for ports that collide: err = %v", err)
	}
	if open := collide.open(); len(open) != 0 {
		t.Errorf("a failed hold left ports %v held", open)
	}
	_, got, err := holdPorts([]int{16379, 16379}, []bool{true, false}, busy().listen)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 16379 || got[1] != 16380 {
		t.Errorf("got %v, want the asked-for port kept and the other moved", got)
	}
	_, got, err = holdPorts([]int{15432, 15433}, []bool{false, true}, busy().listen)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 15434 || got[1] != 15433 {
		t.Errorf("got %v, want the asked-for 15433 reserved before the default port moves past it", got)
	}
}

func TestHoldPortsGivesUpAfterAHundred(t *testing.T) {
	t.Parallel()
	all := &fakePorts{taken: map[int]bool{}}
	for p := 15432; p < 15532; p++ {
		all.taken[p] = true
	}
	if _, _, err := holdPorts([]int{15432}, []bool{false}, all.listen); err == nil || !strings.Contains(err.Error(), "no free local port from 15432 to 15531") {
		t.Errorf("err = %v, want no free port", err)
	}
	all.taken[15531] = false
	if _, got, err := holdPorts([]int{15432}, []bool{false}, all.listen); err != nil || got[0] != 15531 {
		t.Errorf("got %v, %v, want 15531, the last port tried", got, err)
	}
}

func TestFixedPorts(t *testing.T) {
	t.Parallel()
	got := fixedPorts([]connectConfig{{}, {Port: 15432}, {Cmd: []string{"psql"}}})
	if want := []bool{false, true, false}; !slices.Equal(got, want) {
		t.Errorf("fixedPorts = %v, want %v", got, want)
	}
}

func TestValueChecksBeforeAuth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		desc string
		cfg  connectConfig
		want string
	}{
		{"bad --network", connectConfig{Network: "projects/p/networks/n"}, "is not a network"},
		{"bad --subnet path", connectConfig{Subnet: "projects/x/subnetworks/y"}, "cannot parse subnet path"},
		{"bad redis instance path", connectConfig{Service: "redis", Instance: "projects/garbage"}, "cannot parse"},
	} {
		if err := validateValues(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.desc, err, tc.want)
		}
	}
	for _, ok := range []connectConfig{
		{Network: "default"},
		{Network: "projects/host/global/networks/shared"},
		{Subnet: "default"},
		{Subnet: "projects/p/regions/r/subnetworks/s"},
		{Service: "redis", Instance: "us-central1/cache"},
		{Service: "redis", Instance: "projects/p/locations/r/instances/n"},
	} {
		if err := validateValues(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}

func TestPortZero(t *testing.T) {
	t.Parallel()
	if err := checkPortZero(map[string]bool{"p": true}, 0); err == nil {
		t.Error("-p 0 must be refused")
	}
	if err := checkPortZero(map[string]bool{"port": true}, 0); err == nil {
		t.Error("--port 0 must be refused")
	}
	if err := checkPortZero(map[string]bool{}, 0); err != nil {
		t.Errorf("no -p: %v", err)
	}
}

func TestEachTargetIsLookedUpWithItsOwnQuotaProject(t *testing.T) {
	t.Parallel()
	session := quota.WithProject(context.Background(), "prj-first")
	for _, c := range []connectConfig{
		{Project: "prj-first", Service: fakeSOCKS, Instance: "quota-first", NoInput: true},
		{Project: "prj-second", Service: fakeSOCKS, Instance: "quota-second", NoInput: true},
	} {
		if _, err := resolveTarget(session, c, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := quotaProjects.Load(c.Instance); got != c.Project {
			t.Errorf("%s was looked up with quota project %v, want %s", c.Instance, got, c.Project)
		}
	}
}
