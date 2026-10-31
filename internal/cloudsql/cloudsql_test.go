package cloudsql

import (
	"strings"
	"testing"

	"google.golang.org/api/sqladmin/v1"
)

func pscInst(ip, consumerNetwork, dbVersion, region string) *sqladmin.DatabaseInstance {
	return &sqladmin.DatabaseInstance{
		Name:            "inst",
		DatabaseVersion: dbVersion,
		Region:          region,
		Settings: &sqladmin.Settings{
			IpConfiguration: &sqladmin.IpConfiguration{
				PscConfig: &sqladmin.PscConfig{
					PscAutoConnections: []*sqladmin.PscAutoConnectionConfig{
						{IpAddress: ip, ConsumerNetwork: consumerNetwork},
					},
				},
			},
		},
	}
}

func privateInst(ip, network, dbVersion, region string) *sqladmin.DatabaseInstance {
	return &sqladmin.DatabaseInstance{
		Name:            "inst",
		DatabaseVersion: dbVersion,
		Region:          region,
		IpAddresses: []*sqladmin.IpMapping{
			{Type: "PRIVATE", IpAddress: ip},
		},
		Settings: &sqladmin.Settings{
			IpConfiguration: &sqladmin.IpConfiguration{
				PrivateNetwork: network,
			},
		},
	}
}

func publicOnlyInst() *sqladmin.DatabaseInstance {
	return &sqladmin.DatabaseInstance{
		Name:            "inst",
		DatabaseVersion: "POSTGRES_16",
		Region:          "us-central1",
		IpAddresses: []*sqladmin.IpMapping{
			{Type: "PRIMARY", IpAddress: "34.1.2.3"},
		},
		Settings: &sqladmin.Settings{
			IpConfiguration: &sqladmin.IpConfiguration{},
		},
	}
}

func TestEndpointFrom(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		inst     *sqladmin.DatabaseInstance
		wantIP   string
		wantPort int
		wantNP   string
		wantNet  string
		wantErr  bool
	}{
		{
			name: "PSC auto-connection wins over private IP",
			inst: func() *sqladmin.DatabaseInstance {
				inst := pscInst("10.1.0.2", "projects/vpc-project/global/networks/shared-net", "POSTGRES_16", "us-central1")

				inst.IpAddresses = []*sqladmin.IpMapping{
					{Type: "PRIVATE", IpAddress: "10.2.0.5"},
				}
				inst.Settings.IpConfiguration.PrivateNetwork = "projects/other/global/networks/other-net"
				return inst
			}(),
			wantIP:   "10.1.0.2",
			wantPort: 5432,
			wantNP:   "vpc-project",
			wantNet:  "shared-net",
		},
		{
			name: "PSC empty IP falls through to private IP",
			inst: func() *sqladmin.DatabaseInstance {
				inst := pscInst("", "projects/vpc-project/global/networks/net", "MYSQL_8_0", "europe-west1")
				inst.IpAddresses = []*sqladmin.IpMapping{
					{Type: "PRIVATE", IpAddress: "10.3.0.1"},
				}
				inst.Settings.IpConfiguration.PrivateNetwork = "projects/my-proj/global/networks/default"
				return inst
			}(),
			wantIP:   "10.3.0.1",
			wantPort: 3306,
			wantNP:   "my-proj",
			wantNet:  "default",
		},
		{
			name:     "private IP fallback",
			inst:     privateInst("10.0.0.1", "projects/p1/global/networks/n1", "POSTGRES_16", "us-east1"),
			wantIP:   "10.0.0.1",
			wantPort: 5432,
			wantNP:   "p1",
			wantNet:  "n1",
		},
		{
			name:     "shared-VPC: network project differs from instance project",
			inst:     privateInst("10.5.0.1", "projects/host-proj/global/networks/shared-vpc", "SQLSERVER_2022_STANDARD", "us-central1"),
			wantIP:   "10.5.0.1",
			wantPort: 1433,
			wantNP:   "host-proj",
			wantNet:  "shared-vpc",
		},
		{
			name:    "public-only → error",
			inst:    publicOnlyInst(),
			wantErr: true,
		},
		{
			name:     "port: POSTGRES_16",
			inst:     pscInst("10.0.0.1", "projects/p/global/networks/n", "POSTGRES_16", "r"),
			wantIP:   "10.0.0.1",
			wantPort: 5432,
			wantNP:   "p",
			wantNet:  "n",
		},
		{
			name:     "port: MYSQL_8_0",
			inst:     pscInst("10.0.0.1", "projects/p/global/networks/n", "MYSQL_8_0", "r"),
			wantIP:   "10.0.0.1",
			wantPort: 3306,
			wantNP:   "p",
			wantNet:  "n",
		},
		{
			name:     "port: SQLSERVER_2022_STANDARD",
			inst:     pscInst("10.0.0.1", "projects/p/global/networks/n", "SQLSERVER_2022_STANDARD", "r"),
			wantIP:   "10.0.0.1",
			wantPort: 1433,
			wantNP:   "p",
			wantNet:  "n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, err := endpointFrom(tc.inst)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ep.IP != tc.wantIP {
				t.Errorf("IP: got %q, want %q", ep.IP, tc.wantIP)
			}
			if ep.Port != tc.wantPort {
				t.Errorf("Port: got %d, want %d", ep.Port, tc.wantPort)
			}
			if ep.NetworkProject != tc.wantNP {
				t.Errorf("NetworkProject: got %q, want %q", ep.NetworkProject, tc.wantNP)
			}
			if ep.Network != tc.wantNet {
				t.Errorf("Network: got %q, want %q", ep.Network, tc.wantNet)
			}
		})
	}
}

func TestChoiceFor(t *testing.T) {
	tests := []struct {
		desc string
		inst *sqladmin.DatabaseInstance
		want Choice
	}{
		{desc: "primary", inst: &sqladmin.DatabaseInstance{Name: "sql-main", InstanceType: "CLOUD_SQL_INSTANCE"}, want: Choice{Value: "sql-main", Label: "sql-main"}},
		{
			desc: "read replica",
			inst: &sqladmin.DatabaseInstance{Name: "sql-ro", InstanceType: "READ_REPLICA_INSTANCE", MasterInstanceName: "my-project:sql-main"},
			want: Choice{Value: "sql-ro", Label: "sql-ro (read replica of sql-main)"},
		},
		{
			desc: "read replica, primary without a project",
			inst: &sqladmin.DatabaseInstance{Name: "sql-ro", InstanceType: "READ_REPLICA_INSTANCE", MasterInstanceName: "sql-main"},
			want: Choice{Value: "sql-ro", Label: "sql-ro (read replica of sql-main)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := choiceFor(tc.inst); got != tc.want {
				t.Errorf("choiceFor() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestListCmd(t *testing.T) {
	if got, want := ListCmd("p"), "gcloud sql instances list --project p"; got != want {
		t.Errorf("ListCmd = %q, want %q", got, want)
	}
}

func TestAPublicOnlyInstanceEndsWithOneFix(t *testing.T) {
	_, err := endpointFrom(publicOnlyInst())
	want := "\nFix: give the instance a private IP, or let Private Service Connect create an endpoint in your VPC"
	if err == nil || !strings.HasSuffix(err.Error(), want) || strings.Count(err.Error(), "\nFix: ") != 1 {
		t.Errorf("err = %v, want one Fix, last: %q", err, want)
	}
}

func TestClientHintNamesTheEnginesClient(t *testing.T) {
	cases := map[int]string{
		5432: `try: psql "host=127.0.0.1 port=15432 user=postgres"`,
		3306: "try: mysql -h 127.0.0.1 -P 15432 -u root -p",
		1433: "try: sqlcmd -S 127.0.0.1,15432 -U sqlserver -C",
		9999: "",
	}
	for remote, want := range cases {
		if got := clientHint(remote, 15432); got != want {
			t.Errorf("clientHint(%d) = %q, want %q", remote, got, want)
		}
	}
}

func TestOnlyARunningInstanceConnects(t *testing.T) {
	cases := []struct {
		state, policy, wantErr string
	}{
		{state: "RUNNABLE"},
		{state: "ONLINE_MAINTENANCE"},
		{state: "PENDING_CREATE", wantErr: "is not ready yet (state PENDING_CREATE)\nFix: wait until it is RUNNABLE"},
		{state: "MAINTENANCE", wantErr: "is not ready yet (state MAINTENANCE)\nFix: wait until it is RUNNABLE"},
		{state: "SUSPENDED", wantErr: "is not running (state SUSPENDED)\nFix: choose an instance that is RUNNABLE"},
		{state: "FAILED", wantErr: "is not running (state FAILED)"},
		{state: "RUNNABLE", policy: "NEVER", wantErr: "is stopped\nFix: start it with gcloud sql instances patch inst --activation-policy=ALWAYS --project p"},
	}
	for _, tc := range cases {
		inst := privateInst("10.0.0.5", "projects/p/global/networks/vpc", "POSTGRES_16", "us-central1")
		inst.State, inst.Project, inst.Settings.ActivationPolicy = tc.state, "p", tc.policy
		_, err := endpointFrom(inst)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("state %s: unexpected error %v", tc.state, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) || strings.Count(err.Error(), "\nFix: ") != 1 {
			t.Errorf("state %s, policy %q: err = %v, want %q", tc.state, tc.policy, err, tc.wantErr)
		}
	}
}
