package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	schemagen "github.com/estuary/connectors/go-schema-gen"
	boilerplate "github.com/estuary/connectors/source-boilerplate"
	pc "github.com/estuary/flow/go/protocols/capture"
)

type resource struct {
	Path string `json:"path" jsonschema:"title=Path to Collection,description=Supports parent/*/nested to capture all nested collections of parent's children"`
}

func (r resource) Validate() error {
	return nil
}

type config struct {
	// Service account JSON key to use as Application Default Credentials
	CredentialsJSON string `json:"googleCredentials" jsonschema:"title=Credentials,description=Google Cloud Service Account JSON credentials." jsonschema_extras:"secret=true,multiline=true"`

	// Optional name of the database to capture from
	DatabasePath string `json:"database,omitempty" jsonschema:"title=Database,description=Optional name of the database to capture from. Leave blank to autodetect. Typically \"projects/$PROJECTID/databases/(default)\"."`
}

var databasePathRe = regexp.MustCompile(`^projects/[^/]+/databases/[^/]+$`)

func (c *config) Validate() error {
	if c.CredentialsJSON == "" {
		return fmt.Errorf("googleCredentials is required")
	}
	if c.DatabasePath != "" && !databasePathRe.MatchString(c.DatabasePath) {
		return fmt.Errorf("invalid database path %q", c.DatabasePath)
	}
	return nil
}

type driver struct{}

func (driver) Spec(ctx context.Context, req *pc.SpecRequest) (*pc.SpecResponse, error) {
	var endpointSchema, err = schemagen.GenerateSchema("Google Firestore", &config{}).MarshalJSON()
	if err != nil {
		fmt.Println(fmt.Errorf("generating endpoint schema: %w", err))
	}
	resourceSchema, err := schemagen.GenerateSchema("Firestore Resource Spec", &resource{}).MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("generating resource schema: %w", err)
	}

	return &pc.SpecResponse{
		EndpointSpecSchemaJson: json.RawMessage(endpointSchema),
		ResourceSpecSchemaJson: json.RawMessage(resourceSchema),
		DocumentationUrl:       "https://go.estuary.dev/source-firestore",
	}, nil
}

func main() {
	boilerplate.RunMain(new(driver))
}
