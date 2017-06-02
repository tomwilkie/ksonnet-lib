package ksonnet

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/ksonnet/ksonnet-lib/ksonnet-gen/kubespec"
)

// Emit takes a swagger API specification, and returns the text of
// `ksonnet-lib`, written in Jsonnet.
func Emit(spec *kubespec.APISpec) ([]byte, error) {
	root := newRoot(spec)

	m := newMarshaller()
	return root.emit(m)
}

//-----------------------------------------------------------------------------
// Root.
//-----------------------------------------------------------------------------

// `root` is an abstraction of the root of `k8s.libsonnet`, which can be
// emitted as Jsonnet code using the `emit` method.
//
// `root` contains and manages a set of `groups`, which represent a
// set of Kubernetes API groups (e.g., core, apps, extensions), and
// holds all of the logic required to build the `groups` from an
// `kubespec.APISpec`.
type root struct {
	groups groupSet // set of groups, e.g., core, apps, extensions.
}

func newRoot(spec *kubespec.APISpec) *root {
	root := root{
		groups: make(groupSet),
	}

	for defName, def := range spec.Definitions {
		root.addDefinition(defName, def)
	}

	return &root
}

func (root *root) emit(m *marshaller) ([]byte, error) {
	m.bufferLine("{")
	m.indent()

	// Emit in sorted order so that we can diff the output.
	for _, group := range root.groups.toSortedSlice() {
		group.emit(m, root)
	}

	m.dedent()
	m.bufferLine("}")

	return m.writeAll()
}

func (root *root) addDefinition(
	name kubespec.DefinitionName, def *kubespec.SchemaDefinition,
) {
	parsedName := name.Parse()
	isTopLevel := len(def.TopLevelSpecs) > 0
	apiObject, err := root.getOrCreateAPIObject(parsedName, isTopLevel)
	if err != nil {
		return
	}

	for propName, prop := range def.Properties {
		pm := newPropertyMethod(propName, prop, apiObject)
		apiObject.propertyMethods[propName] = pm
	}
}

func (root *root) getOrCreateAPIObject(
	parsedName *kubespec.ParsedDefinitionName, isTopLevel bool,
) (*apiObject, error) {
	if parsedName.Version == nil {
		return nil, fmt.Errorf(
			"Can't make API object from name with nil version. package type: '%d' kind: '%s'",
			parsedName.PackageType,
			parsedName.Kind)
	}

	var groupName kubespec.GroupName
	if parsedName.Group == nil {
		groupName = "core"
	} else {
		groupName = *parsedName.Group
	}

	group, ok := root.groups[groupName]
	if !ok {
		group = newGroup(groupName, root)
		root.groups[groupName] = group
	}

	versionedAPI, ok := group.versionedAPIs[*parsedName.Version]
	if !ok {
		versionedAPI = newVersionedAPI(*parsedName.Version, group)
		group.versionedAPIs[*parsedName.Version] = versionedAPI
	}

	apiObject, ok := versionedAPI.apiObjects[parsedName.Kind]
	if ok {
		log.Fatalf("Duplicate object kinds with name '%s'", parsedName.Unparse())
	}
	apiObject = newAPIObject(parsedName.Kind, versionedAPI, isTopLevel)
	versionedAPI.apiObjects[parsedName.Kind] = apiObject
	return apiObject, nil
}

func (root *root) getAPIObject(
	parsedName *kubespec.ParsedDefinitionName,
) (*apiObject, error) {
	if parsedName.Version == nil {
		return nil, fmt.Errorf(
			"Can't make API object from name with nil version. package type: '%d' kind: '%s'",
			parsedName.PackageType,
			parsedName.Kind)
	}

	var groupName kubespec.GroupName
	if parsedName.Group == nil {
		groupName = "core"
	} else {
		groupName = *parsedName.Group
	}

	group, ok := root.groups[groupName]
	if !ok {
		return nil, fmt.Errorf(
			"Could not retrieve object, group '%s' doesn't exist", groupName)
	}

	versionedAPI, ok := group.versionedAPIs[*parsedName.Version]
	if !ok {
		return nil, fmt.Errorf(
			"Could not retrieve object, versioned API '%s' doesn't exist",
			*parsedName.Version)
	}

	if apiObject, ok := versionedAPI.apiObjects[parsedName.Kind]; ok {
		return apiObject, nil
	}
	return nil, fmt.Errorf(
		"Could not retrieve object, kind '%s' doesn't exist",
		parsedName.Kind)
}

//-----------------------------------------------------------------------------
// Group.
//-----------------------------------------------------------------------------

// `group` is an abstract representation of a Kubernetes API group
// (e.g., apps, extensions, core), which can be emitted as Jsonnet
// code using the `emit` method.
//
// `group` contains a set of versioned APIs (e.g., v1, v1beta1, etc.),
// though the logic for creating them is handled largely by `root`.
type group struct {
	name          kubespec.GroupName // e.g., core, apps, extensions.
	versionedAPIs versionedAPISet    // e.g., v1, v1beta1.
	parent        *root
}
type groupSet map[kubespec.GroupName]*group
type groupSlice []*group

func newGroup(name kubespec.GroupName, parent *root) *group {
	return &group{
		name:          name,
		versionedAPIs: make(versionedAPISet),
		parent:        parent,
	}
}

func (group *group) emit(m *marshaller, root *root) error {
	line := fmt.Sprintf("%s:: {", group.name)
	m.bufferLine(line)
	m.indent()

	// Emit in sorted order so that we can diff the output.
	for _, versioned := range group.versionedAPIs.toSortedSlice() {
		versioned.emit(m, root)
	}

	m.dedent()
	m.bufferLine("},")
	return nil
}

func (gs groupSet) toSortedSlice() groupSlice {
	groups := groupSlice{}
	for _, group := range gs {
		groups = append(groups, group)
	}
	sort.Sort(groups)
	return groups
}

//
// sort.Interface implementation for groupSlice.
//

func (gs groupSlice) Len() int {
	return len(gs)
}
func (gs groupSlice) Swap(i, j int) {
	gs[i], gs[j] = gs[j], gs[i]
}
func (gs groupSlice) Less(i, j int) bool {
	return gs[i].name < gs[j].name
}

//-----------------------------------------------------------------------------
// Versioned API.
//-----------------------------------------------------------------------------

// `versionedAPI` is an abstract representation of a version of a
// Kubernetes API group (e.g., apps.v1beta1, extensions.v1beta1,
// core.v1), which can be emitted as Jsonnet code using the `emit`
// method.
//
// `versionedAPI` contains a set of API objects (e.g., v1.Container,
// v1beta1.Deployment, etc.), though the logic for creating them is
// handled largely by `root`.
type versionedAPI struct {
	version    kubespec.VersionString // version string, e.g., v1, v1beta1.
	apiObjects apiObjectSet           // set of objects, e.g, v1.Container.
	parent     *group
}
type versionedAPISet map[kubespec.VersionString]*versionedAPI
type versionedAPISlice []*versionedAPI

func newVersionedAPI(
	version kubespec.VersionString, parent *group,
) *versionedAPI {
	return &versionedAPI{
		version:    version,
		apiObjects: make(apiObjectSet),
		parent:     parent,
	}
}

func (va *versionedAPI) emit(m *marshaller, root *root) error {
	line := fmt.Sprintf("%s:: {", va.version)
	m.bufferLine(line)
	m.indent()

	// Emit in sorted order so that we can diff the output.
	for _, object := range va.apiObjects.toSortedSlice() {
		if !object.isTopLevel {
			continue
		}
		object.emit(m, root)
	}

	m.dedent()
	m.bufferLine("},")
	return nil
}

func (vas versionedAPISet) toSortedSlice() versionedAPISlice {
	versionedAPIs := versionedAPISlice{}
	for _, va := range vas {
		versionedAPIs = append(versionedAPIs, va)
	}
	sort.Sort(versionedAPIs)
	return versionedAPIs
}

//
// sort.Interface implementation for versionedAPISlice.
//

func (vas versionedAPISlice) Len() int {
	return len(vas)
}
func (vas versionedAPISlice) Swap(i, j int) {
	vas[i], vas[j] = vas[j], vas[i]
}
func (vas versionedAPISlice) Less(i, j int) bool {
	return vas[i].version < vas[j].version
}

//-----------------------------------------------------------------------------
// API object.
//-----------------------------------------------------------------------------

// `apiObject` is an abstract representation of a Kubernetes API
// object (e.g., v1.Container, v1beta1.Deployment), which can be
// emitted as Jsonnet code using the `emit` method.
//
// `apiObject` contains a set of property methods and mixins which
// formulate the basis of much of ksonnet-lib's programming surface.
// The logic for creating them is handled largely by `root`.
type apiObject struct {
	name            kubespec.ObjectKind // e.g., `Container` in `v1.Container`
	propertyMethods propertyMethodSet   // e.g., container.image, container.env
	parent          *versionedAPI
	isTopLevel      bool
}
type apiObjectSet map[kubespec.ObjectKind]*apiObject
type apiObjectSlice []*apiObject

func newAPIObject(
	name kubespec.ObjectKind, parent *versionedAPI, isTopLevel bool,
) *apiObject {
	return &apiObject{
		name:            name,
		propertyMethods: make(propertyMethodSet),
		parent:          parent,
		isTopLevel:      isTopLevel,
	}
}

func (ao *apiObject) emit(m *marshaller, root *root) error {
	jsonnetName := toJsonnetName(ao.name)
	if _, ok := ao.parent.apiObjects[jsonnetName]; ok {
		log.Fatalf(
			"Tried to lowercase first character of object kind '%s', but lowercase name was already present in version '%s'",
			jsonnetName,
			ao.parent.version)
	}

	line := fmt.Sprintf("%s:: {", jsonnetName)
	m.bufferLine(line)
	m.indent()

	for _, pm := range ao.propertyMethods.toSortedSlice() {
		if isSpecialProperty(pm.name) {
			continue
		}
		pm.emit(m, root)
	}

	m.dedent()
	m.bufferLine("},")
	return nil
}

func (aos apiObjectSet) toSortedSlice() apiObjectSlice {
	apiObjects := apiObjectSlice{}
	for _, apiObject := range aos {
		apiObjects = append(apiObjects, apiObject)
	}
	sort.Sort(apiObjects)
	return apiObjects
}

//
// sort.Interface implementation for apiObjectSlice.
//

func (aos apiObjectSlice) Len() int {
	return len(aos)
}
func (aos apiObjectSlice) Swap(i, j int) {
	aos[i], aos[j] = aos[j], aos[i]
}
func (aos apiObjectSlice) Less(i, j int) bool {
	return aos[i].name < aos[j].name
}

//-----------------------------------------------------------------------------
// Property method.
//-----------------------------------------------------------------------------

// `propertyMethod` is an abstract representation of a ksonnet-lib's
// property methods, which can be emitted as Jsonnet code using the
// `emit` method.
//
// For example, ksonnet-lib exposes many functions such as
// `v1.container.image`, which can be added together with the `+`
// operator to construct a complete image. `propertyMethod` is an
// abstract representation of these so-called "property methods".
//
// `propertyMethod` contains the name of the property given in the
// `apiObject` that is its parent (for example, `Deployment` has a
// field called `containers`, which is an array of `v1.Container`), as
// well as the `kubespec.PropertyName`, which contains information
// required to generate the Jsonnet code.
//
// The logic for creating them is handled largely by `root`.
type propertyMethod struct {
	*kubespec.Property
	name   kubespec.PropertyName // e.g., image in container.image.
	parent *apiObject
}
type propertyMethodSet map[kubespec.PropertyName]*propertyMethod
type propertyMethodSlice []*propertyMethod

func newPropertyMethod(
	name kubespec.PropertyName, property *kubespec.Property, parent *apiObject,
) *propertyMethod {
	return &propertyMethod{
		Property: property,
		name:     name,
		parent:   parent,
	}
}

func (pm *propertyMethod) emit(m *marshaller, root *root) {
	paramName := pm.name
	fieldName := pm.name
	signature := fmt.Sprintf("%s(%s)::", pm.name, paramName)

	if pm.Ref != nil {
		defn := "#/definitions/"
		ref := string(*pm.Ref)
		if !strings.HasPrefix(ref, defn) {
			log.Fatalln(ref)
		}
		// TODO: Emit code for property methods that take refs as args,
		// and generate mixins.
	} else if pm.Type != nil {
		paramType := *pm.Type
		var body string
		if paramType == "array" {
			body = fmt.Sprintf(
				"if std.type(%s) == \"array\" then {%s+: %s} else {%s: [%s]}",
				paramName,
				fieldName,
				paramName,
				fieldName,
				paramName)
		} else {
			body = fmt.Sprintf("{%s+: %s}", paramName, fieldName)
		}

		line := fmt.Sprintf("%s %s,", signature, body)
		m.bufferLine(line)
	} else {
		log.Fatalf("Neither a type nor a ref")
	}
}

func (aos propertyMethodSet) toSortedSlice() propertyMethodSlice {
	apiObjects := propertyMethodSlice{}
	for _, apiObject := range aos {
		apiObjects = append(apiObjects, apiObject)
	}
	sort.Sort(apiObjects)
	return apiObjects
}

//
// sort.Interface implementation for propertyMethodSlice.
//

func (pms propertyMethodSlice) Len() int {
	return len(pms)
}
func (pms propertyMethodSlice) Swap(i, j int) {
	pms[i], pms[j] = pms[j], pms[i]
}
func (pms propertyMethodSlice) Less(i, j int) bool {
	return pms[i].name < pms[j].name
}
