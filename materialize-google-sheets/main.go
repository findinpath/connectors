package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	google_auth "github.com/estuary/connectors/go-auth/google"
	schemagen "github.com/estuary/connectors/go-schema-gen"
	boilerplate "github.com/estuary/connectors/materialize-boilerplate"
	pf "github.com/estuary/flow/go/protocols/flow"
	pm "github.com/estuary/flow/go/protocols/materialize"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// Scopes required for OAuth authentication
var scopes = []string{sheets.SpreadsheetsScope}

// driver implements the pm.DriverServer interface.
type driver struct{}

type config struct {
	SpreadsheetURL string                        `json:"spreadsheetUrl" jsonschema:"title=Spreadsheet URL"`
	Credentials    *google_auth.CredentialConfig `json:"credentials" jsonschema:"title=Authentication"`
}

func (config) GetFieldDocString(fieldName string) string {
	switch fieldName {
	case "SpreadsheetURL":
		return "URL of the spreadsheet to materialize into."
	default:
		return ""
	}
}

// Validate returns an error if the config is not well-formed.
func (c config) Validate() error {
	if _, err := parseSheetsID(c.SpreadsheetURL); err != nil {
		return err
	}

	return c.Credentials.Validate()
}

func (c config) spreadsheetID() string {
	var id, err = parseSheetsID(c.SpreadsheetURL)
	if err != nil {
		panic(err)
	}
	return id
}

func (c config) buildService(ctx context.Context) (*sheets.Service, error) {
	creds, err := c.Credentials.GoogleCredentials(ctx, scopes...)
	if err != nil {
		return nil, fmt.Errorf("building sheets service: %w", err)
	}

	client, err := sheets.NewService(context.Background(), option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("building sheets service: %w", err)
	}
	return client, nil
}

type resource struct {
	Sheet string `json:"sheet" jsonschema:"title=Sheet Name" jsonschema_extras:"x-collection-name=true"`
}

func (resource) GetFieldDocString(fieldName string) string {
	switch fieldName {
	case "Sheet":
		return "Name of the spreadsheet sheet to materialize into."
	default:
		return ""
	}
}

func (r resource) Validate() error {
	if r.Sheet == "" {
		return fmt.Errorf("missing required sheet name")
	}
	return nil
}

type driverCheckpoint struct {
	Round int64
}

func (c driverCheckpoint) Validate() error {
	return nil
}

func (driver) Spec(ctx context.Context, req *pm.SpecRequest) (*pm.SpecResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("validating request: %w", err)
	}

	es := schemagen.GenerateSchema("Google Sheets Materialization", &config{})
	endpointSchema, err := es.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("generating endpoint schema: %w", err)
	}

	resourceSchema, err := schemagen.GenerateSchema("Google Sheets Materialization Binding", &resource{}).MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("generating resource schema: %w", err)
	}

	return &pm.SpecResponse{
		EndpointSpecSchemaJson: json.RawMessage(endpointSchema),
		ResourceSpecSchemaJson: json.RawMessage(resourceSchema),
		DocumentationUrl:       "https://go.estuary.dev/materialize-google-sheets",
		Oauth2Spec:             google_auth.Spec(scopes...),
	}, nil
}

func (driver) Validate(ctx context.Context, req *pm.ValidateRequest) (*pm.ValidateResponse, error) {
	var cfg config
	if err := pf.UnmarshalStrict(req.EndpointSpecJson, &cfg); err != nil {
		return nil, err
	}

	var svc, err = cfg.buildService(ctx)
	if err != nil {
		return nil, err
	} else if _, err = loadSheetIDMapping(svc, cfg.spreadsheetID()); err != nil {
		return nil, fmt.Errorf("verifying credentials: %w", err)
	}

	var out []*pm.ValidateResponse_Binding
	for _, binding := range req.Bindings {

		var res resource
		if err := pf.UnmarshalStrict(binding.ResourceSpecJson, &res); err != nil {
			return nil, fmt.Errorf("parsing resource config: %w", err)
		}

		var constraints = make(map[string]*pm.Constraint)
		for _, projection := range binding.Collection.Projections {
			var constraint = new(pm.Constraint)
			switch {
			case projection.IsRootDocumentProjection():
				constraint.Type = pm.Constraint_LOCATION_REQUIRED
				constraint.Reason = "The root document must be materialized"
			case projection.IsPrimaryKey:
				constraint.Type = pm.Constraint_LOCATION_REQUIRED
				constraint.Reason = "Components of the collection key must be materialized"
			case strings.HasPrefix(projection.Field, "_meta"):
				constraint.Type = pm.Constraint_FIELD_OPTIONAL
				constraint.Reason = "Metadata fields are optional"
			case projection.Inference.IsSingleScalarType():
				constraint.Type = pm.Constraint_LOCATION_RECOMMENDED
				constraint.Reason = "Scalar types are recommended for materialization"
			default:
				constraint.Type = pm.Constraint_FIELD_OPTIONAL
				constraint.Reason = "Field is optional"
			}
			constraints[projection.Field] = constraint
		}

		out = append(out, &pm.ValidateResponse_Binding{
			Constraints:  constraints,
			DeltaUpdates: false,
			ResourcePath: []string{res.Sheet},
		})
	}

	return &pm.ValidateResponse{Bindings: out}, nil
}

func (d driver) ApplyUpsert(ctx context.Context, req *pm.ApplyRequest) (*pm.ApplyResponse, error) {
	return d.apply(ctx, req, false)
}

func (d driver) ApplyDelete(ctx context.Context, req *pm.ApplyRequest) (*pm.ApplyResponse, error) {
	return d.apply(ctx, req, true)
}

func (driver) apply(ctx context.Context, req *pm.ApplyRequest, isDelete bool) (*pm.ApplyResponse, error) {
	var cfg config
	if err := pf.UnmarshalStrict(req.Materialization.EndpointSpecJson, &cfg); err != nil {
		return nil, err
	}

	var svc, err = cfg.buildService(ctx)
	if err != nil {
		return nil, err
	}
	sheetIDs, err := loadSheetIDMapping(svc, cfg.spreadsheetID())
	if err != nil {
		return nil, err
	}

	var actions []*sheets.Request
	var description string
	var rand = rand.New(rand.NewSource(time.Now().UnixMicro()))

	for _, binding := range req.Materialization.Bindings {
		var res resource
		if err := pf.UnmarshalStrict(binding.ResourceSpecJson, &res); err != nil {
			return nil, fmt.Errorf("parsing resource config: %w", err)
		}
		var _, exists = sheetIDs[res.Sheet]

		if !exists && !isDelete {
			description += fmt.Sprintf("Created sheet %q.\n", res.Sheet)

			// Create a new sheet.
			var sheetID = int64(rand.Int31())
			actions = append(actions, &sheets.Request{
				AddSheet: &sheets.AddSheetRequest{
					Properties: &sheets.SheetProperties{
						Title:   res.Sheet,
						SheetId: sheetID,
					},
				},
			})
		} else if exists && isDelete {
			description += fmt.Sprintf("Deleted sheet %q.\n", res.Sheet)

			actions = append(actions, &sheets.Request{
				DeleteSheet: &sheets.DeleteSheetRequest{
					SheetId: sheetIDs[res.Sheet],
				},
			})
		}
	}

	if req.DryRun || len(actions) == 0 {
		// Nothing to do.
	} else if err = batchRequestWithRetry(ctx, svc, cfg.spreadsheetID(), actions); err != nil {
		return nil, fmt.Errorf("while updated sheets: %w", err)
	}

	return &pm.ApplyResponse{ActionDescription: description}, nil
}

// Transactions implements the DriverServer interface.
func (driver) Transactions(stream pm.Driver_TransactionsServer) error {
	return pm.RunTransactions(stream, func(ctx context.Context, open pm.TransactionRequest_Open) (pm.Transactor, *pm.TransactionResponse_Opened, error) {
		var cfg config
		if err := pf.UnmarshalStrict(open.Materialization.EndpointSpecJson, &cfg); err != nil {
			return nil, nil, fmt.Errorf("parsing endpoint config: %w", err)
		}

		var checkpoint driverCheckpoint
		if err := pf.UnmarshalStrict(open.DriverCheckpointJson, &checkpoint); err != nil {
			return nil, nil, fmt.Errorf("parsing driver checkpoint: %w", err)
		}

		svc, err := cfg.buildService(stream.Context())
		if err != nil {
			return nil, nil, err
		}

		states, err := loadSheetStates(
			open.Materialization.Bindings,
			svc,
			cfg.spreadsheetID(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("recovering sheet states: %w", err)
		}

		if err := writeSheetSentinels(stream.Context(), svc, cfg.spreadsheetID(), states); err != nil {
			return nil, nil, fmt.Errorf("writing sheet sentinels: %w", err)
		}

		bindings, err := buildTransactorBindings(
			open.Materialization.Bindings,
			checkpoint.Round,
			states,
		)
		if err != nil {
			return nil, nil, err
		}

		if err := writeSheetHeaders(stream.Context(), svc, cfg.spreadsheetID(), bindings); err != nil {
			return nil, nil, fmt.Errorf("writing sheet headers: %w", err)
		}

		var transactor = &transactor{
			bindings:      bindings,
			client:        svc,
			round:         checkpoint.Round,
			spreadsheetId: cfg.spreadsheetID(),
		}
		return transactor, &pm.TransactionResponse_Opened{}, nil
	})
}

func main() { boilerplate.RunMain(new(driver)) }
