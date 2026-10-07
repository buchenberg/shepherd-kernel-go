package shepherd

import (
	"errors"
	"fmt"
)

// Schema-library boundary for record builders and pure projections
// (plan 02 §4), mirroring shepherd2/schemas/schema_library.py.
//
// A projection declares what it needs from a slice; the store's read path
// decides what a slice carries. The gap between the two is where silent
// wrong answers come from — a captures_only slice fed to a projection that
// folds declarations would quietly project an empty run — so the check is
// explicit and loud.

// ProjectionModeRequirement is what a projection needs a slice's mode filter
// to be. "any" accepts everything.
type ProjectionModeRequirement string

const (
	ProjectionRequiresDeclarations ProjectionModeRequirement = "declarations_only"
	ProjectionRequiresCaptures     ProjectionModeRequirement = "captures_only"
	ProjectionRequiresBoth         ProjectionModeRequirement = "both"
	ProjectionAcceptsAny           ProjectionModeRequirement = "any"
)

// ProjectionSpec declares one pure projection's kernel requirements.
type ProjectionSpec struct {
	Name            string
	ModeRequirement ProjectionModeRequirement
	RequiresPayload bool
	AcceptsAnchors  bool
}

// SchemaLibrary is the minimal contract for schema-defined records and
// projections.
type SchemaLibrary interface {
	Name() string
	SchemaRefs() []string
	ProjectionSpecs() []ProjectionSpec
}

// StaticSchemaLibrary is a simple immutable schema-library descriptor.
type StaticSchemaLibrary struct {
	LibraryName string
	Refs        []string
	Specs       []ProjectionSpec
}

func (l *StaticSchemaLibrary) Name() string                      { return l.LibraryName }
func (l *StaticSchemaLibrary) SchemaRefs() []string              { return l.Refs }
func (l *StaticSchemaLibrary) ProjectionSpecs() []ProjectionSpec { return l.Specs }

// ErrProjectionMode reports a slice whose mode filter contradicts a
// projection's declared requirement. Python raises ProjectionModeError (a
// ValueError subclass); Go has no error hierarchy here, so the sentinel is
// matched with errors.Is.
var ErrProjectionMode = errors.New("shepherd: projection mode incompatible")

// ensureProjectionCompatible validates that a slice satisfies a projection's
// declared kernel requirements. It returns an error wrapping
// ErrProjectionMode when the mode filter disagrees.
func ensureProjectionCompatible(slice Slice, spec ProjectionSpec) error {
	if spec.ModeRequirement == ProjectionAcceptsAny {
		return nil
	}
	if slice.ModeFilter != ModeFilter(spec.ModeRequirement) {
		return fmt.Errorf("%w: projection %q requires mode_filter=%q; got %q",
			ErrProjectionMode, spec.Name, spec.ModeRequirement, slice.ModeFilter)
	}
	return nil
}
