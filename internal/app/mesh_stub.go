//go:build !mesh

package app

import (
	"doge/internal/agent"
	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/mesh"
	"doge/internal/web"
)

// newMesh returns no mesh: this binary was built without -tags mesh.
func newMesh(mesh.Options) (mesh.Mesh, []*agent.AgentTool) {
	return nil, nil
}

// attachMeshCollab is a no-op without the mesh build.
func attachMeshCollab(mesh.Mesh, *coding.AgentSession, []*agent.AgentTool, *uploads.Store) web.MeshView {
	return nil
}
