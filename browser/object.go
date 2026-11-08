package browser

import "github.com/Tangerg/frame"

// Object references a value in its original Runtime. Its Go representation
// cannot implement the referenced value's native language contract.
type Object struct {
	Reference ObjectRef
}

func (o ObjectRef) validIn(instanceID string) bool {
	return frame.ValidName(instanceID) && o.Export.InstanceID == instanceID &&
		frame.ValidName(o.ObjectID) && frame.ValidName(o.Export.Name) && o.Export.Contract.Validate() == nil
}

func (*Bridge) ValidateExport(instanceID string, export frame.ExportValue) error {
	object, ok := export.Value.(Object)
	if !ok || !object.Reference.validIn(instanceID) ||
		object.Reference.Export.Name != export.Name || object.Reference.Export.Contract != export.Contract {
		return frame.ErrContractMismatch
	}
	return nil
}
