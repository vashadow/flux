package release

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/ghodss/yaml"
	"github.com/go-kit/kit/log"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/helm/pkg/chartutil"
	k8shelm "k8s.io/helm/pkg/helm"
	hapi_release "k8s.io/helm/pkg/proto/hapi/release"

	"github.com/weaveworks/flux"
	fluxk8s "github.com/weaveworks/flux/cluster/kubernetes"
	flux_v1beta1 "github.com/weaveworks/flux/integrations/apis/flux.weave.works/v1beta1"
	helmutil "k8s.io/helm/pkg/releaseutil"
)

type Action string

const (
	InstallAction Action = "CREATE"
	UpgradeAction Action = "UPDATE"
)

// Release contains clients needed to provide functionality related to helm releases
type Release struct {
	logger     log.Logger
	HelmClient *k8shelm.Client
}

type Releaser interface {
	GetDeployedRelease(name string) (*hapi_release.Release, error)
	Install(dir string, releaseName string, fhr flux_v1beta1.HelmRelease, action Action, opts InstallOptions) (*hapi_release.Release, error)
}

type DeployInfo struct {
	Name string
}

type InstallOptions struct {
	DryRun    bool
	ReuseName bool
}

// New creates a new Release instance.
func New(logger log.Logger, helmClient *k8shelm.Client) *Release {
	r := &Release{
		logger:     logger,
		HelmClient: helmClient,
	}
	return r
}

// GetReleaseName either retrieves the release name from the Custom Resource or constructs a new one
// in the form : $Namespace-$CustomResourceName
func GetReleaseName(fhr flux_v1beta1.HelmRelease) string {
	namespace := fhr.Namespace
	if namespace == "" {
		namespace = "default"
	}
	releaseName := fhr.Spec.ReleaseName
	if releaseName == "" {
		releaseName = fmt.Sprintf("%s-%s", namespace, fhr.Name)
	}

	return releaseName
}

// GetDeployedRelease returns a release with Deployed status
func (r *Release) GetDeployedRelease(name string) (*hapi_release.Release, error) {
	rls, err := r.HelmClient.ReleaseContent(name)
	if err != nil {
		return nil, err
	}
	if rls.Release.Info.Status.GetCode() == hapi_release.Status_DEPLOYED {
		return rls.GetRelease(), nil
	}
	return nil, nil
}

func (r *Release) canDelete(name string) (bool, error) {
	rls, err := r.HelmClient.ReleaseStatus(name)

	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Error finding status for release (%s): %#v", name, err))
		return false, err
	}
	/*
		"UNKNOWN":          0,
		"DEPLOYED":         1,
		"DELETED":          2,
		"SUPERSEDED":       3,
		"FAILED":           4,
		"DELETING":         5,
		"PENDING_INSTALL":  6,
		"PENDING_UPGRADE":  7,
		"PENDING_ROLLBACK": 8,
	*/
	status := rls.GetInfo().GetStatus()
	switch status.Code {
	case 1, 4:
		r.logger.Log("info", fmt.Sprintf("Deleting release %s", name))
		return true, nil
	case 2:
		r.logger.Log("info", fmt.Sprintf("Release %s already deleted", name))
		return false, nil
	default:
		r.logger.Log("info", fmt.Sprintf("Release %s with status %s cannot be deleted", name, status.Code.String()))
		return false, fmt.Errorf("release %s with status %s cannot be deleted", name, status.Code.String())
	}
}

// Install performs a Chart release given the directory containing the
// charts, and the HelmRelease specifying the release. Depending
// on the release type, this is either a new release, or an upgrade of
// an existing one.
//
// TODO(michael): cloneDir is only relevant if installing from git;
// either split this procedure into two varieties, or make it more
// general and calculate the path to the chart in the caller.
func (r *Release) Install(chartPath, releaseName string, fhr flux_v1beta1.HelmRelease, action Action, opts InstallOptions, kubeClient *kubernetes.Clientset) (*hapi_release.Release, error) {
	if chartPath == "" {
		return nil, fmt.Errorf("empty path to chart supplied for resource %q", fhr.ResourceID().String())
	}
	_, err := os.Stat(chartPath)
	switch {
	case os.IsNotExist(err):
		return nil, fmt.Errorf("no file or dir at path to chart: %s", chartPath)
	case err != nil:
		return nil, fmt.Errorf("error statting path given for chart %s: %s", chartPath, err.Error())
	}

	r.logger.Log("info", fmt.Sprintf("processing release %s (as %s)", fhr.Spec.ReleaseName, releaseName),
		"action", fmt.Sprintf("%v", action),
		"options", fmt.Sprintf("%+v", opts),
		"timeout", fmt.Sprintf("%vs", fhr.GetTimeout()))

	// Read values from given valueFile paths (configmaps, etc.)
	mergedValues := chartutil.Values{}
	for _, valueFileSecret := range fhr.Spec.ValueFileSecrets {
		// Read the contents of the secret
		secret, err := kubeClient.CoreV1().Secrets(fhr.Namespace).Get(valueFileSecret.Name, v1.GetOptions{})
		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Cannot get secret %s for Chart release [%s]: %#v", valueFileSecret.Name, fhr.Spec.ReleaseName, err))
			return nil, err
		}

		// Load values.yaml file and merge
		var values chartutil.Values
		err = yaml.Unmarshal(secret.Data["values.yaml"], &values)
		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Cannot yaml.Unmashal values.yaml in secret %s for Chart release [%s]: %#v", valueFileSecret.Name, fhr.Spec.ReleaseName, err))
			return nil, err
		}
		mergedValues = mergeValues(mergedValues, values)
	}
	// Merge in values after valueFiles
	mergedValues = mergeValues(mergedValues, fhr.Spec.Values)

	strVals, err := mergedValues.YAML()
	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Problem with supplied customizations for Chart release [%s]: %#v", fhr.Spec.ReleaseName, err))
		return nil, err
	}
	rawVals := []byte(strVals)

	switch action {
	case InstallAction:
		res, err := r.HelmClient.InstallRelease(
			chartPath,
			fhr.GetNamespace(),
			k8shelm.ValueOverrides(rawVals),
			k8shelm.ReleaseName(releaseName),
			k8shelm.InstallDryRun(opts.DryRun),
			k8shelm.InstallReuseName(opts.ReuseName),
			k8shelm.InstallTimeout(fhr.GetTimeout()),
		)

		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Chart release failed: %s: %#v", fhr.Spec.ReleaseName, err))
			// purge the release if the install failed but only if this is the first revision
			history, err := r.HelmClient.ReleaseHistory(releaseName, k8shelm.WithMaxHistory(2))
			if err == nil && len(history.Releases) == 1 && history.Releases[0].Info.Status.Code == hapi_release.Status_FAILED {
				r.logger.Log("info", fmt.Sprintf("Deleting failed release: [%s]", fhr.Spec.ReleaseName))
				_, err = r.HelmClient.DeleteRelease(releaseName, k8shelm.DeletePurge(true))
				if err != nil {
					r.logger.Log("error", fmt.Sprintf("Release deletion error: %#v", err))
					return nil, err
				}
			}
			return nil, err
		}
		if !opts.DryRun {
			r.annotateResources(res.Release, fhr)
		}
		return res.Release, err
	case UpgradeAction:
		res, err := r.HelmClient.UpdateRelease(
			releaseName,
			chartPath,
			k8shelm.UpdateValueOverrides(rawVals),
			k8shelm.UpgradeDryRun(opts.DryRun),
			k8shelm.UpgradeTimeout(fhr.GetTimeout()),
			k8shelm.ResetValues(fhr.Spec.ResetValues),
		)

		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Chart upgrade release failed: %s: %#v", fhr.Spec.ReleaseName, err))
			return nil, err
		}
		if !opts.DryRun {
			r.annotateResources(res.Release, fhr)
		}
		return res.Release, err
	default:
		err = fmt.Errorf("Valid install options: CREATE, UPDATE. Provided: %s", action)
		r.logger.Log("error", err.Error())
		return nil, err
	}
}

// Delete purges a Chart release
func (r *Release) Delete(name string) error {
	ok, err := r.canDelete(name)
	if !ok {
		if err != nil {
			return err
		}
		return nil
	}

	_, err = r.HelmClient.DeleteRelease(name, k8shelm.DeletePurge(true))
	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Release deletion error: %#v", err))
		return err
	}
	r.logger.Log("info", fmt.Sprintf("Release deleted: [%s]", name))
	return nil
}

// annotateResources annotates each of the resources created (or updated)
// by the release so that we can spot them.
func (r *Release) annotateResources(release *hapi_release.Release, fhr flux_v1beta1.HelmRelease) {
	manifests := helmutil.SplitManifests(release.Manifest)

	var objs []unstructured.Unstructured
	for _, manifest := range manifests {
		json, err := yaml.YAMLToJSON([]byte(manifest))
		if err != nil {
			r.logger.Log("err", err)
			continue
		}

		var u unstructured.Unstructured
		if err := u.UnmarshalJSON(json); err != nil {
			r.logger.Log("err", err)
		}

		// Helm charts may include list kinds, we are only interested in
		// the items on those lists.
		if u.IsList() {
			l, err := u.ToList()
			if err != nil {
				r.logger.Log("err", err)
			}
			objs = append(objs, l.Items...)
			continue
		}

		objs = append(objs, u)
	}

	resources := make(map[string][]string)
	for _, obj := range objs {
		namespace := obj.GetNamespace()
		if namespace == "" {
			namespace = release.Namespace
		}
		resource := obj.GetKind() + "/" + obj.GetName()
		resources[namespace] = append(resources[namespace], resource)
	}

	for namespace, res := range resources {
		args := []string{"annotate", "--overwrite"}
		args = append(args, "--namespace", namespace)
		args = append(args, res...)
		args = append(args, fluxk8s.AntecedentAnnotation+"="+fhrResourceID(fhr).String())

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "kubectl", args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			r.logger.Log("output", string(output), "err", err)
		}
	}
}

// fhrResourceID constructs a flux.ResourceID for a HelmRelease resource.
func fhrResourceID(fhr flux_v1beta1.HelmRelease) flux.ResourceID {
	return flux.MakeResourceID(fhr.Namespace, "HelmRelease", fhr.Name)
}

// Merges source and destination `chartutils.Values`, preferring values from the source Values
// This is slightly adapted from https://github.com/helm/helm/blob/master/cmd/helm/install.go#L329
func mergeValues(dest, src chartutil.Values) chartutil.Values {
	for k, v := range src {
		// If the key doesn't exist already, then just set the key to that value
		if _, exists := dest[k]; !exists {
			dest[k] = v
			continue
		}
		nextMap, ok := v.(map[string]interface{})
		// If it isn't another map, overwrite the value
		if !ok {
			dest[k] = v
			continue
		}
		// Edge case: If the key exists in the destination, but isn't a map
		destMap, isMap := dest[k].(map[string]interface{})
		// If the source map has a map for this key, prefer it
		if !isMap {
			dest[k] = v
			continue
		}
		// If we got to this point, it is a map in both, so merge them
		dest[k] = mergeValues(destMap, nextMap)
	}
	return dest
}
