// Package host owns plugin installation, exact provider binding and admission.
// Catalog, preparation and Use transitions share one private Host owner; execution
// lifetimes supply resource guarantees without owning selection or authorization.
package host
