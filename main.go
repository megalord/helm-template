package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/spf13/cobra"

	"k8s.io/helm/cmd/helm/strvals"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/engine"
	"k8s.io/helm/pkg/hooks"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/release"
	util "k8s.io/helm/pkg/releaseutil"
	"k8s.io/helm/pkg/timeconv"
)

const globalUsage = `
Render chart templates locally and display the output.

This does not require Tiller. However, any values that would normally be
looked up or retrieved in-cluster will be faked locally. Additionally, none
of the server-side testing of chart validity (e.g. whether an API is supported)
is done.
`

var (
	setVals     string
	valsFiles   valueFiles
	flagVerbose bool
	showNotes   bool
)

func main() {
	cmd := &cobra.Command{
		Use:   "template [flags] CHART",
		Short: "locally render templates",
		Long:  globalUsage,
		RunE:  run,
	}

	f := cmd.Flags()
	f.StringVar(&setVals, "set", "", "set values on the command line. See 'helm install -h'")
	f.VarP(&valsFiles, "values", "f", "specify one or more YAML files of values")
	f.BoolVarP(&flagVerbose, "verbose", "v", false, "show the computed YAML values as well.")
	f.BoolVar(&showNotes, "notes", false, "show the computed NOTES.txt file as well.")

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return errors.New("chart is required")
	}
	c, err := chartutil.Load(args[0])
	if err != nil {
		return err
	}

	vv, err := vals()
	if err != nil {
		return err
	}

	config := &chart.Config{Raw: string(vv), Values: map[string]*chart.Value{}}

	if flagVerbose {
		fmt.Println("---\n# merged values")
		fmt.Println(string(vv))
	}

	options := chartutil.ReleaseOptions{
		Name:      "RELEASE-NAME",
		Time:      timeconv.Now(),
		Namespace: "NAMESPACE",
		//Revision:  1,
		//IsInstall: true,
	}

	// Set up engine.
	renderer := engine.New()

	vals, err := chartutil.ToRenderValues(c, config, options)
	if err != nil {
		return err
	}

	out, err := renderer.Render(c, vals)
	if err != nil {
		return err
	}

  // out is a map[string]string{}
  // func sortManifests(files map[string]string, apis chartutil.VersionSet, sort SortOrder) ([]*release.Hook, []manifest, error) {
  _, manifests, err := sortManifests(out, chartutil.DefaultVersionSet, InstallOrder)

  for _, m := range manifests {
		b := filepath.Base(m.name)
		if !showNotes && b == "NOTES.txt" {
			continue
		}
		if strings.HasPrefix(b, "_") {
			continue
		}
		fmt.Printf("---\n# Source: %s\n", m.name)
		fmt.Println(m.content)
	}
	return nil
}

// liberally borrows from Helm
func vals() ([]byte, error) {
	base := map[string]interface{}{}

	// User specified a values files via -f/--values
	for _, filePath := range valsFiles {
		currentMap := map[string]interface{}{}
		bytes, err := ioutil.ReadFile(filePath)
		if err != nil {
			return []byte{}, err
		}

		if err := yaml.Unmarshal(bytes, &currentMap); err != nil {
			return []byte{}, fmt.Errorf("failed to parse %s: %s", filePath, err)
		}
		// Merge with the previous map
		base = mergeValues(base, currentMap)
	}

	if err := strvals.ParseInto(setVals, base); err != nil {
		return []byte{}, fmt.Errorf("failed parsing --set data: %s", err)
	}

	return yaml.Marshal(base)
}

// Copied from Helm.

func mergeValues(dest map[string]interface{}, src map[string]interface{}) map[string]interface{} {
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
		// If the key doesn't exist already, then just set the key to that value
		if _, exists := dest[k]; !exists {
			dest[k] = nextMap
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

type valueFiles []string

func (v *valueFiles) String() string {
	return fmt.Sprint(*v)
}

func (v *valueFiles) Type() string {
	return "valueFiles"
}

func (v *valueFiles) Set(value string) error {
	for _, filePath := range strings.Split(value, ",") {
		*v = append(*v, filePath)
	}
	return nil
}


// pkg/tiller/hooks.go
var events = map[string]release.Hook_Event{
	hooks.PreInstall:         release.Hook_PRE_INSTALL,
	hooks.PostInstall:        release.Hook_POST_INSTALL,
	hooks.PreDelete:          release.Hook_PRE_DELETE,
	hooks.PostDelete:         release.Hook_POST_DELETE,
	hooks.PreUpgrade:         release.Hook_PRE_UPGRADE,
	hooks.PostUpgrade:        release.Hook_POST_UPGRADE,
	hooks.PreRollback:        release.Hook_PRE_ROLLBACK,
	hooks.PostRollback:       release.Hook_POST_ROLLBACK,
	hooks.ReleaseTestSuccess: release.Hook_RELEASE_TEST_SUCCESS,
	hooks.ReleaseTestFailure: release.Hook_RELEASE_TEST_FAILURE,
}

// manifest represents a manifest file, which has a name and some content.
type manifest struct {
	name    string
	content string
	head    *util.SimpleHead
}

// sortManifests takes a map of filename/YAML contents and sorts them into hook types.
//
// The resulting hooks struct will be populated with all of the generated hooks.
// Any file that does not declare one of the hook types will be placed in the
// 'generic' bucket.
//
// To determine hook type, this looks for a YAML structure like this:
//
//  kind: SomeKind
//  apiVersion: v1
// 	metadata:
//		annotations:
//			helm.sh/hook: pre-install
//
// Where HOOK_NAME is one of the known hooks.
//
// If a file declares more than one hook, it will be copied into all of the applicable
// hook buckets. (Note: label keys are not unique within the labels section).
//
// Files that do not parse into the expected format are simply placed into a map and
// returned.
func sortManifests(files map[string]string, apis chartutil.VersionSet, sort SortOrder) ([]*release.Hook, []manifest, error) {
	hs := []*release.Hook{}
	generic := []manifest{}

	for n, c := range files {
		// Skip partials. We could return these as a separate map, but there doesn't
		// seem to be any need for that at this time.
		if strings.HasPrefix(path.Base(n), "_") {
			continue
		}
		// Skip empty files, and log this.
		if len(strings.TrimSpace(c)) == 0 {
			continue
		}

		var sh util.SimpleHead
		err := yaml.Unmarshal([]byte(c), &sh)

		if err != nil {
			e := fmt.Errorf("YAML parse error on %s: %s", n, err)
			return hs, generic, e
		}

		if sh.Version != "" && !apis.Has(sh.Version) {
			return hs, generic, fmt.Errorf("apiVersion %q in %s is not available", sh.Version, n)
		}

		if sh.Metadata == nil || sh.Metadata.Annotations == nil || len(sh.Metadata.Annotations) == 0 {
			generic = append(generic, manifest{name: n, content: c, head: &sh})
			continue
		}

		hookTypes, ok := sh.Metadata.Annotations[hooks.HookAnno]
		if !ok {
			generic = append(generic, manifest{name: n, content: c, head: &sh})
			continue
		}
		h := &release.Hook{
			Name:     sh.Metadata.Name,
			Kind:     sh.Kind,
			Path:     n,
			Manifest: c,
			Events:   []release.Hook_Event{},
		}

		isHook := false
		for _, hookType := range strings.Split(hookTypes, ",") {
			hookType = strings.ToLower(strings.TrimSpace(hookType))
			e, ok := events[hookType]
			if ok {
				isHook = true
				h.Events = append(h.Events, e)
			}
		}

		if !isHook {
			continue
		}
		hs = append(hs, h)
	}
	return hs, sortByKind(generic, sort), nil
}



// pkg/tiller/kind_sorter.go

// SortOrder is an ordering of Kinds.
type SortOrder []string

// InstallOrder is the order in which manifests should be installed (by Kind).
var InstallOrder SortOrder = []string{
  "Project",
	"Secret",
	"ConfigMap",
	"PersistentVolume",
	"PersistentVolumeClaim",
	"ServiceAccount",
	"ClusterRole",
	"ClusterRoleBinding",
	"Role",
	"RoleBinding",
  "ImageStream",
	"Service",
  "BuildConfig",
	"Pod",
	"ReplicationController",
	"Deployment",
  "DeploymentConfig",
	"DaemonSet",
	"Ingress",
	"Job",
}

// sortByKind does an in-place sort of manifests by Kind.
//
// Results are sorted by 'ordering'
func sortByKind(manifests []manifest, ordering SortOrder) []manifest {
	ks := newKindSorter(manifests, ordering)
	sort.Sort(ks)
	return ks.manifests
}

type kindSorter struct {
	ordering  map[string]int
	manifests []manifest
}

func newKindSorter(m []manifest, s SortOrder) *kindSorter {
	o := make(map[string]int, len(s))
	for v, k := range s {
		o[k] = v
	}

	return &kindSorter{
		manifests: m,
		ordering:  o,
	}
}

func (k *kindSorter) Len() int { return len(k.manifests) }

func (k *kindSorter) Swap(i, j int) { k.manifests[i], k.manifests[j] = k.manifests[j], k.manifests[i] }

func (k *kindSorter) Less(i, j int) bool {
	a := k.manifests[i]
	b := k.manifests[j]
	first, ok := k.ordering[a.head.Kind]
	if !ok {
		// Unknown is always last
		return false
	}
	second, ok := k.ordering[b.head.Kind]
	if !ok {
		return true
	}
	return first < second
}
