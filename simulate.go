package main

import (
	"context"
	"path/filepath"
	"sort"

	kustomv1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type sourceMap map[types.NamespacedName]*sourcev1.GitRepository

type scenario interface {
	findAffectedSources(context.Context, client.Client) (sourceMap, error)
	artifactForSource(context.Context, string, client.Client, *sourcev1.GitRepository) (string, error)
}

func simulate(ctx context.Context, tmp string, scenario scenario, k8sClient client.WithWatch) error {
	reposOfInterest, err := scenario.findAffectedSources(ctx, k8sClient)
	if err != nil {
		return err
	}

	// Find which Kustomizations depend on those
	var kustoms kustomv1.KustomizationList
	if err = k8sClient.List(ctx, &kustoms, &client.ListOptions{}); err != nil {
		return err
	}

	type kustomAndSource struct {
		kustom *kustomv1.Kustomization
		source *sourcev1.GitRepository
	}

	var kustomsToApply []kustomAndSource
	for i := range kustoms.Items {
		kustom := &kustoms.Items[i]
		// TODO check if ready i.e., viable?
		sourceRef := kustom.Spec.SourceRef
		if sourceRef.Kind == "GitRepository" { // FIXME APIVersion too
			repoName := types.NamespacedName{
				Name:      sourceRef.Name,
				Namespace: sourceRef.Namespace,
			}
			if repoName.Namespace == "" {
				repoName.Namespace = kustom.GetNamespace()
			}

			if src, ok := reposOfInterest[repoName]; ok {
				nsn := client.ObjectKeyFromObject(kustom)
				log.V(INFO).Info("including Kustomization using GitRepository", "name", client.ObjectKeyFromObject(kustom), "source name", nsn)
				kustomsToApply = append(kustomsToApply, kustomAndSource{
					kustom: kustom,
					source: src,
				})
			}
		}
	}

	// sort (stably) so that the Kustomizations can be output in gorups under each source (and in the same order for each run, if nothing else changes).
	sort.SliceStable(kustomsToApply, func(i, j int) bool {
		srci, srcj := kustomsToApply[i].source, kustomsToApply[j].source
		if srci.GetNamespace() == srcj.GetNamespace() {
			return srci.GetName() < srcj.GetName()
		}
		return srci.GetNamespace() < srcj.GetNamespace()
	})

	var currentSource *sourcev1.GitRepository

	// Simulate each of those with the content of the new branch
	for _, ks := range kustomsToApply {
		if ks.source != currentSource {
			currentSource = ks.source
			println("GitRepository", client.ObjectKeyFromObject(currentSource).String(), "changed per the scenario")
		}

		//   Do the Kustomization dry-run, like `flux diff kustomization`,
		//   putting any changes to Flux objects onto a queue to be
		//   simulated.

		kustom := ks.kustom
		repo := ks.source

		artifactdir, err := scenario.artifactForSource(ctx, tmp, k8sClient, repo)
		if err != nil {
			return err
		}

		kustomizedir := filepath.Join(artifactdir, kustom.Spec.Path) // FIXME separators
		println("  Kustomization", client.ObjectKeyFromObject(kustom).String())
		diffs, err := dryrunKustomization(ctx, k8sClient, kustom, kustomizedir)
		if err != nil {
			return err
		}
		printDiffs(diffs)
	}

	return nil
}
