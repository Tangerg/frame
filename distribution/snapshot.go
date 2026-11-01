package distribution

import "slices"

func (u Unit) snapshot() Unit {
	u.Imports = slices.Clone(u.Imports)
	u.Exports = slices.Clone(u.Exports)
	u.Resources = slices.Clone(u.Resources)
	u.RequestedCapabilities = slices.Clone(u.RequestedCapabilities)
	u.Variants = slices.Clone(u.Variants)
	for i, v := range u.Variants {
		if v.Runtime != nil {
			config := *v.Runtime
			u.Variants[i].Runtime = &config
		}
	}
	if u.Module != nil {
		module := *u.Module
		module.Imports = slices.Clone(module.Imports)
		for i, v := range module.Imports {
			if v.Shared != nil {
				ref := *v.Shared
				module.Imports[i].Shared = &ref
			}
		}
		u.Module = &module
	}
	return u
}

func (r Release) snapshot() Release {
	r.Manifest.Artifacts = slices.Clone(r.Manifest.Artifacts)
	r.Manifest.Contracts = slices.Clone(r.Manifest.Contracts)
	r.Manifest.Dependencies = slices.Clone(r.Manifest.Dependencies)
	r.Manifest.Units = slices.Clone(r.Manifest.Units)
	for i, u := range r.Manifest.Units {
		r.Manifest.Units[i] = u.snapshot()
	}
	r.Manifest.Contributions = slices.Clone(r.Manifest.Contributions)
	for i, c := range r.Manifest.Contributions {
		r.Manifest.Contributions[i].Metadata = slices.Clone(c.Metadata)
	}
	r.Lock.Dependencies = slices.Clone(r.Lock.Dependencies)
	r.Lock.Artifacts = slices.Clone(r.Lock.Artifacts)
	r.Lock.Contracts = slices.Clone(r.Lock.Contracts)
	return r
}
