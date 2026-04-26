package scim

import (
	"encoding/json"
	"net/http"
)

// SCIM 2.0 discovery endpoints (RFC 7644 §4).
//
// Discovery endpoints let SCIM clients introspect the provider's
// supported features, schemas, and resource types without
// hard-coding implementation details. They are the first thing
// the SCIM 2.0 reference test suite hits when validating a
// provider; without them, conformance fails before any CRUD
// check runs.
//
// The endpoints below are unauthenticated — RFC 7644 explicitly
// allows discovery to be public, and Okta / Azure AD / JumpCloud
// all hit them without a bearer token.

// SchemaListResponseSchemas is the schema URI for the schemas
// endpoint response (the same ListResponse schema as Users / Groups).
const SchemaServiceProviderConfig = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
const SchemaResourceType = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
const SchemaSchema = "urn:ietf:params:scim:schemas:core:2.0:Schema"

// ServiceProviderConfig is the response body for
// `GET /scim/v2/ServiceProviderConfig`. Fields mirror RFC 7644
// §5; values describe the subset KMail actually implements.
type ServiceProviderConfig struct {
	Schemas               []string                  `json:"schemas"`
	DocumentationURI      string                    `json:"documentationUri,omitempty"`
	Patch                 SupportedFeature          `json:"patch"`
	Bulk                  BulkConfig                `json:"bulk"`
	Filter                FilterConfig              `json:"filter"`
	ChangePassword        SupportedFeature          `json:"changePassword"`
	Sort                  SupportedFeature          `json:"sort"`
	ETag                  SupportedFeature          `json:"etag"`
	AuthenticationSchemes []AuthenticationScheme    `json:"authenticationSchemes"`
	Meta                  Meta                      `json:"meta"`
}

// SupportedFeature is RFC 7644 §5's `{"supported": <bool>}` shape.
type SupportedFeature struct {
	Supported bool `json:"supported"`
}

// BulkConfig describes bulk operation limits.
type BulkConfig struct {
	Supported      bool `json:"supported"`
	MaxOperations  int  `json:"maxOperations,omitempty"`
	MaxPayloadSize int  `json:"maxPayloadSize,omitempty"`
}

// FilterConfig describes filter support.
type FilterConfig struct {
	Supported  bool `json:"supported"`
	MaxResults int  `json:"maxResults,omitempty"`
}

// AuthenticationScheme is one entry in the auth schemes list.
type AuthenticationScheme struct {
	Type             string `json:"type"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	SpecURI          string `json:"specUri,omitempty"`
	DocumentationURI string `json:"documentationUri,omitempty"`
}

// ResourceType is one entry in the `/ResourceTypes` response.
type ResourceType struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Endpoint    string   `json:"endpoint"`
	Description string   `json:"description"`
	Schema      string   `json:"schema"`
	Meta        Meta     `json:"meta"`
}

// SchemaAttribute is one row of a Schema resource's attributes.
type SchemaAttribute struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	MultiValued  bool   `json:"multiValued"`
	Required     bool   `json:"required"`
	CaseExact    bool   `json:"caseExact"`
	Mutability   string `json:"mutability"`
	Returned     string `json:"returned"`
	Uniqueness   string `json:"uniqueness,omitempty"`
}

// SchemaResource is one entry in the `/Schemas` response.
type SchemaResource struct {
	Schemas     []string          `json:"schemas"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Attributes  []SchemaAttribute `json:"attributes"`
	Meta        Meta              `json:"meta"`
}

// serviceProviderConfig builds the response for
// `GET /scim/v2/ServiceProviderConfig`. Values reflect what KMail
// actually implements — PATCH supported, bulk / filter / sort /
// changePassword / etag NOT supported (the conformance matrix in
// `docs/SCIM_CONFORMANCE.md` documents these decisions).
func serviceProviderConfig() ServiceProviderConfig {
	return ServiceProviderConfig{
		Schemas:          []string{SchemaServiceProviderConfig},
		DocumentationURI: "https://github.com/kennguy3n/kmail/blob/main/docs/SCIM_CONFORMANCE.md",
		Patch:            SupportedFeature{Supported: true},
		Bulk:             BulkConfig{Supported: false},
		Filter:           FilterConfig{Supported: false},
		ChangePassword:   SupportedFeature{Supported: false},
		Sort:             SupportedFeature{Supported: false},
		ETag:             SupportedFeature{Supported: false},
		AuthenticationSchemes: []AuthenticationScheme{
			{
				Type:        "oauthbearertoken",
				Name:        "OAuth Bearer Token",
				Description: "Per-tenant SCIM bearer token issued by the KMail admin UI.",
				SpecURI:     "https://www.rfc-editor.org/rfc/rfc6750",
			},
		},
		Meta: Meta{
			ResourceType: "ServiceProviderConfig",
			Location:     "/scim/v2/ServiceProviderConfig",
		},
	}
}

// resourceTypes returns the SCIM resource types KMail exposes.
func resourceTypes() []ResourceType {
	return []ResourceType{
		{
			Schemas:     []string{SchemaResourceType},
			ID:          "User",
			Name:        "User",
			Endpoint:    "/Users",
			Description: "User Account",
			Schema:      SchemaUser,
			Meta:        Meta{ResourceType: "ResourceType", Location: "/scim/v2/ResourceTypes/User"},
		},
		{
			Schemas:     []string{SchemaResourceType},
			ID:          "Group",
			Name:        "Group",
			Endpoint:    "/Groups",
			Description: "Group (mapped to KMail shared inboxes)",
			Schema:      SchemaGroup,
			Meta:        Meta{ResourceType: "ResourceType", Location: "/scim/v2/ResourceTypes/Group"},
		},
	}
}

// schemas returns the SCIM Schemas KMail exposes. Attribute lists
// reflect the subset of Core 2.0 User/Group fields the service
// actually persists or projects through `projectUser` /
// `projectGroup`.
func schemas() []SchemaResource {
	return []SchemaResource{
		{
			Schemas:     []string{SchemaSchema},
			ID:          SchemaUser,
			Name:        "User",
			Description: "User Account",
			Attributes: []SchemaAttribute{
				{Name: "userName", Type: "string", Required: true, CaseExact: false, Mutability: "readWrite", Returned: "default", Uniqueness: "server"},
				{Name: "displayName", Type: "string", Mutability: "readWrite", Returned: "default"},
				{Name: "active", Type: "boolean", Mutability: "readWrite", Returned: "default"},
				{Name: "emails", Type: "complex", MultiValued: true, Mutability: "readWrite", Returned: "default"},
				{Name: "name", Type: "complex", Mutability: "readWrite", Returned: "default"},
				{Name: "externalId", Type: "string", Mutability: "readWrite", Returned: "default"},
			},
			Meta: Meta{ResourceType: "Schema", Location: "/scim/v2/Schemas/" + SchemaUser},
		},
		{
			Schemas:     []string{SchemaSchema},
			ID:          SchemaGroup,
			Name:        "Group",
			Description: "Group (KMail shared inbox)",
			Attributes: []SchemaAttribute{
				{Name: "displayName", Type: "string", Required: true, Mutability: "readWrite", Returned: "default"},
				{Name: "members", Type: "complex", MultiValued: true, Mutability: "readWrite", Returned: "default"},
			},
			Meta: Meta{ResourceType: "Schema", Location: "/scim/v2/Schemas/" + SchemaGroup},
		},
	}
}

// HandleServiceProviderConfig serves the SCIM 2.0 service provider
// configuration. Public — no auth required (RFC 7644 §4).
func HandleServiceProviderConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", ContentType)
	_ = json.NewEncoder(w).Encode(serviceProviderConfig())
}

// HandleResourceTypes serves the SCIM 2.0 resource type list.
func HandleResourceTypes(w http.ResponseWriter, _ *http.Request) {
	rs := resourceTypes()
	res := make([]any, 0, len(rs))
	for _, r := range rs {
		res = append(res, r)
	}
	w.Header().Set("Content-Type", ContentType)
	_ = json.NewEncoder(w).Encode(ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: len(res),
		StartIndex:   1,
		ItemsPerPage: len(res),
		Resources:    res,
	})
}

// handleResourceType serves a single resource type by ID.
func handleResourceType(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, rt := range resourceTypes() {
		if rt.ID == id {
			w.Header().Set("Content-Type", ContentType)
			_ = json.NewEncoder(w).Encode(rt)
			return
		}
	}
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(Error{
		Schemas: []string{SchemaError},
		Status:  "404",
		Detail:  "ResourceType not found",
	})
}

// handleSchema serves a single schema by ID.
func handleSchema(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, s := range schemas() {
		if s.ID == id {
			w.Header().Set("Content-Type", ContentType)
			_ = json.NewEncoder(w).Encode(s)
			return
		}
	}
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(Error{
		Schemas: []string{SchemaError},
		Status:  "404",
		Detail:  "Schema not found",
	})
}

// HandleSchemas serves the SCIM 2.0 schemas list.
func HandleSchemas(w http.ResponseWriter, _ *http.Request) {
	ss := schemas()
	res := make([]any, 0, len(ss))
	for _, s := range ss {
		res = append(res, s)
	}
	w.Header().Set("Content-Type", ContentType)
	_ = json.NewEncoder(w).Encode(ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: len(res),
		StartIndex:   1,
		ItemsPerPage: len(res),
		Resources:    res,
	})
}
