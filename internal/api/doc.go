// Package api is the job contract between pogfarm and clipper: the shared
// request and response types, the typed client, and the shared-secret
// authentication used on both sides.
//
// It is the only place where internal/pogfarm and internal/clipper meet; they
// must never import each other. A change to the contract updates both sides
// in the same pull request.
package api
