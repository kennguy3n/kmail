// Package scim implements a minimal SCIM 2.0 provisioning surface
// (RFC 7643 / 7644) layered on top of the existing tenant Service.
//
// The implementation is intentionally narrow: KMail only needs the
// subset its IdP partners (Okta, Azure AD, Google Workspace,
// JumpCloud) actually exercise — Users CRUD, Groups CRUD, and
// PATCH for `active` / membership flips. Vendor-specific schema
// extensions (Okta's `urn:ietf:params:scim:schemas:extension:enterprise:2.0:User`)
// are accepted on input but not persisted.
package scim

import (
	"encoding/json"
	"strings"
	"time"
)

// Schema URIs per RFC 7643.
const (
	SchemaUser         = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaGroup        = "urn:ietf:params:scim:schemas:core:2.0:Group"
	SchemaListResponse = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaPatchOp      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	SchemaError        = "urn:ietf:params:scim:api:messages:2.0:Error"
)

// ContentType is the SCIM-specified response content type.
const ContentType = "application/scim+json"

// Meta is the metadata block every SCIM resource carries.
type Meta struct {
	ResourceType string    `json:"resourceType"`
	Created      time.Time `json:"created"`
	LastModified time.Time `json:"lastModified"`
	Location     string    `json:"location"`
	Version      string    `json:"version,omitempty"`
}

// Email is one row of a User's `emails` list.
type Email struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
	Type    string `json:"type,omitempty"`
}

// Name is the structured-name complex attribute.
type Name struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

// User is the SCIM Core User resource.
type User struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id,omitempty"`
	ExternalID  string   `json:"externalId,omitempty"`
	UserName    string   `json:"userName"`
	DisplayName string   `json:"displayName,omitempty"`
	Active      bool     `json:"active"`
	Emails      []Email  `json:"emails,omitempty"`
	Name        *Name    `json:"name,omitempty"`
	Meta        Meta     `json:"meta"`
}

// GroupMember is one row of a Group's `members` list.
type GroupMember struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
	Type    string `json:"type,omitempty"`
	Ref     string `json:"$ref,omitempty"`
}

// Group is the SCIM Core Group resource. KMail maps groups to
// shared inboxes — a tenant's `shared_inboxes` table is the
// canonical Group store.
type Group struct {
	Schemas     []string      `json:"schemas"`
	ID          string        `json:"id,omitempty"`
	DisplayName string        `json:"displayName"`
	Members     []GroupMember `json:"members,omitempty"`
	Meta        Meta          `json:"meta"`
}

// ListResponse is the paginated wrapper SCIM clients expect on
// list endpoints.
type ListResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []any    `json:"Resources"`
}

// Error is the SCIM error envelope (RFC 7644 §3.12).
type Error struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// PatchRequest is the SCIM PATCH body.
type PatchRequest struct {
	Schemas    []string         `json:"schemas"`
	Operations []PatchOperation `json:"Operations"`
}

// PatchOperation is one op inside a PatchRequest.
type PatchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// PrimaryEmail returns the first email marked primary, or the
// first email present, or empty string.
func PrimaryEmail(emails []Email) string {
	for _, e := range emails {
		if e.Primary {
			return strings.TrimSpace(e.Value)
		}
	}
	if len(emails) > 0 {
		return strings.TrimSpace(emails[0].Value)
	}
	return ""
}
