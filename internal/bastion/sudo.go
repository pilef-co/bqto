package bastion

import (
	"context"
	"slices"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/pilef-co/bqto/gcperr"
	"github.com/pilef-co/bqto/quota"
)

const sudoPermission = "compute.instances.osAdminLogin"

func HasSudo(ctx context.Context, project, zone, name string) (bool, error) {
	c, err := compute.NewInstancesRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return false, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()

	resp, err := gcperr.Retry(ctx, func() (*computepb.TestPermissionsResponse, error) {
		return c.TestIamPermissions(ctx, &computepb.TestIamPermissionsInstanceRequest{
			Project:                        project,
			Zone:                           zone,
			Resource:                       name,
			TestPermissionsRequestResource: &computepb.TestPermissionsRequest{Permissions: []string{sudoPermission}},
		})
	})
	if err != nil {
		return false, gcperr.Wrap("check sudo on bastion "+name, withoutURL(err))
	}
	return slices.Contains(resp.GetPermissions(), sudoPermission), nil
}
