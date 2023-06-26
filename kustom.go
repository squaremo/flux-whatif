package main

import (
	"context"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	"github.com/squaremo/flux-whatif/build"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func dryrunKustomization(ctx context.Context, k8sClient client.WithWatch, kustom *kustomizev1.Kustomization, artifactdir string) ([]build.DiffEntry, error) {
	b, err := build.NewBuilder(kustom.GetName(), artifactdir,
		build.WithKustomization(kustom),
		build.WithClient(k8sClient),
		build.WithDryRun(true),
		build.WithNamespace(kustom.GetNamespace()),
	)
	if err != nil {
		return nil, err
	}
	return b.Diff()
}

func printDiffs(diffs []build.DiffEntry) {
	for _, d := range diffs {
		println(d.Meta.String(), d.Action)
		if d.Action == "update" {
			println(d.Diff)
		}
	}
}
