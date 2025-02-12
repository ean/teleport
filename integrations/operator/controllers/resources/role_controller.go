/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package resources

import (
	"context"

	"github.com/gravitational/trace"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/types"
	resourcesv5 "github.com/gravitational/teleport/integrations/operator/apis/resources/v5"
)

// roleClient implements TeleportResourceClient and offers CRUD methods needed to reconcile roles
type roleClient struct {
	teleportClient *client.Client
}

// Get gets the Teleport role of a given name
func (r roleClient) Get(ctx context.Context, name string) (types.Role, error) {
	role, err := r.teleportClient.GetRole(ctx, name)
	return role, trace.Wrap(err)
}

// Create creates a Teleport role
func (r roleClient) Create(ctx context.Context, role types.Role) error {
	_, err := r.teleportClient.CreateRole(ctx, role)
	return trace.Wrap(err)
}

// Update updates a Teleport role
func (r roleClient) Update(ctx context.Context, role types.Role) error {
	_, err := r.teleportClient.UpdateRole(ctx, role)
	return trace.Wrap(err)
}

// Delete deletes a Teleport role
func (r roleClient) Delete(ctx context.Context, name string) error {
	return trace.Wrap(r.teleportClient.DeleteRole(ctx, name))
}

// NewRoleReconciler instantiates a new Kubernetes controller reconciling role resources
func NewRoleReconciler(client kclient.Client, tClient *client.Client) (Reconciler, error) {
	roleClient := &roleClient{
		teleportClient: tClient,
	}

	resourceReconciler, err := NewTeleportResourceReconciler[types.Role, *resourcesv5.TeleportRole](
		client,
		roleClient,
	)

	return resourceReconciler, trace.Wrap(err, "building teleport resource reconciler")
}
