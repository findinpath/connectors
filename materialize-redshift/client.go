package main

import (
	"context"
	stdsql "database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	awsHttp "github.com/aws/smithy-go/transport/http"
	boilerplate "github.com/estuary/connectors/materialize-boilerplate"
	sql "github.com/estuary/connectors/materialize-sql"
	pf "github.com/estuary/flow/go/protocols/flow"
	"github.com/google/uuid"
	"github.com/jackc/pgconn"

	_ "github.com/jackc/pgx/v4/stdlib"
)

type client struct {
	db  *stdsql.DB
	cfg *config
	ep  *sql.Endpoint
}

func newClient(ctx context.Context, ep *sql.Endpoint) (sql.Client, error) {
	cfg := ep.Config.(*config)

	db, err := stdsql.Open("pgx", cfg.toURI())
	if err != nil {
		return nil, err
	}

	return &client{
		db:  db,
		cfg: cfg,
		ep:  ep,
	}, nil
}

func (c *client) InfoSchema(ctx context.Context, resourcePaths [][]string) (*boilerplate.InfoSchema, error) {
	catalog := c.cfg.Database
	if catalog == "" {
		// An endpoint-level database configuration is not required, so query for the active
		// database if that's the case.
		if err := c.db.QueryRowContext(ctx, "select current_database();").Scan(&catalog); err != nil {
			return nil, fmt.Errorf("querying for connected database: %w", err)
		}
	}

	return sql.StdFetchInfoSchema(ctx, c.db, c.ep.Dialect, catalog, c.cfg.metaSchema(), resourcePaths)
}

func (c *client) PutSpec(ctx context.Context, updateSpec sql.MetaSpecsUpdate) error {
	_, err := c.db.ExecContext(ctx, updateSpec.ParameterizedQuery, updateSpec.Parameters...)
	return err
}

func (c *client) CreateTable(ctx context.Context, tc sql.TableCreate) error {
	_, err := c.db.ExecContext(ctx, tc.TableCreateSql)
	return err
}

func (c *client) ReplaceTable(ctx context.Context, tr sql.TableReplace) (string, boilerplate.ActionApplyFn, error) {
	return tr.TableReplaceSql, func(ctx context.Context) error {
		_, err := c.db.ExecContext(ctx, tr.TableReplaceSql)
		return err
	}, nil
}

func (c *client) AlterTable(ctx context.Context, ta sql.TableAlter) (string, boilerplate.ActionApplyFn, error) {
	// Redshift only allows a single column to be added per ALTER TABLE statement. Also, we will
	// never need to drop nullability constraints, since Redshift does not allow dropping
	// nullability and we don't ever create columns as NOT NULL as a result.
	if len(ta.DropNotNulls) != 0 { // sanity check
		return "", nil, fmt.Errorf("redshift cannot drop nullability constraints but got %d DropNotNulls", len(ta.DropNotNulls))
	}

	statements := []string{}
	for _, c := range ta.AddColumns {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE %s ADD COLUMN %s %s;",
			ta.Identifier,
			c.Identifier,
			c.NullableDDL,
		))
	}

	// Each table column addition statement is a separate statement, but will be grouped
	// together in a single multi-statement query.
	alterStmt := strings.Join(statements, "\n")

	return alterStmt, func(ctx context.Context) error {
		_, err := c.db.ExecContext(ctx, alterStmt)
		return err
	}, nil
}

func (c *client) PreReqs(ctx context.Context) *sql.PrereqErr {
	errs := &sql.PrereqErr{}

	// Use a reasonable timeout for this connection test. It is not uncommon for a misconfigured
	// connection (wrong host, wrong port, etc.) to hang for several minutes on Ping and we want to
	// bail out well before then.
	pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	if err := c.db.PingContext(pingCtx); err != nil {
		// Provide a more user-friendly representation of some common error causes.
		var pgErr *pgconn.PgError
		var netConnErr *net.DNSError
		var netOpErr *net.OpError

		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "28000":
				err = fmt.Errorf("incorrect username or password")
			case "3D000":
				err = fmt.Errorf("database %q does not exist", c.cfg.Database)
			}
		} else if errors.As(err, &netConnErr) {
			if netConnErr.IsNotFound {
				err = fmt.Errorf("host at address %q cannot be found", c.cfg.Address)
			}
		} else if errors.As(err, &netOpErr) {
			if netOpErr.Timeout() {
				errStr := `connection to host at address %q timed out, possible causes:
	* Redshift endpoint is not set to be publicly accessible
	* there is no inbound rule allowing Estuary's IP address to connect through the Redshift VPC security group
	* the configured address is incorrect, possibly with an incorrect host or port
	* if connecting through an SSH tunnel, the SSH bastion server may not be operational, or the connection details are incorrect`
				err = fmt.Errorf(errStr, c.cfg.Address)
			}
		}

		errs.Err(err)
	}

	s3client, err := c.cfg.toS3Client(ctx)
	if err != nil {
		// This is not caused by invalid S3 credentials, and would most likely be a logic error in
		// the connector code.
		errs.Err(err)
		return errs
	}

	// Test creating, reading, and deleting an object from the configured bucket and bucket path.
	testKey := path.Join(c.cfg.BucketPath, uuid.NewString())

	var awsErr *awsHttp.ResponseError
	if _, err := s3client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.cfg.Bucket),
		Key:    aws.String(testKey),
		Body:   strings.NewReader("testing"),
	}); err != nil {
		if errors.As(err, &awsErr) {
			// Handling for the two most common cases: The bucket doesn't exist, or the bucket does
			// exist but the configured credentials aren't authorized to write to it.
			if awsErr.Response.Response.StatusCode == http.StatusNotFound {
				err = fmt.Errorf("bucket %q does not exist", c.cfg.Bucket)
			} else if awsErr.Response.Response.StatusCode == http.StatusForbidden {
				err = fmt.Errorf("not authorized to write to %q", path.Join(c.cfg.Bucket, c.cfg.BucketPath))
			}
		}
		errs.Err(err)
	} else if _, err := s3client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.cfg.Bucket),
		Key:    aws.String(testKey),
	}); err != nil {
		if errors.As(err, &awsErr) && awsErr.Response.Response.StatusCode == http.StatusForbidden {
			err = fmt.Errorf("not authorized to read from %q", path.Join(c.cfg.Bucket, c.cfg.BucketPath))
		}
		errs.Err(err)
	} else if _, err := s3client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.cfg.Bucket),
		Key:    aws.String(testKey),
	}); err != nil {
		if errors.As(err, &awsErr) && awsErr.Response.Response.StatusCode == http.StatusForbidden {
			err = fmt.Errorf("not authorized to delete from %q", path.Join(c.cfg.Bucket, c.cfg.BucketPath))
		}
		errs.Err(err)
	}

	return errs
}

func (c *client) FetchSpecAndVersion(ctx context.Context, specs sql.Table, materialization pf.Materialization) (string, string, error) {
	specHex, version, err := sql.StdFetchSpecAndVersion(ctx, c.db, specs, materialization)
	if err != nil {
		return "", "", err
	}

	specBytes, err := hex.DecodeString(specHex)
	if err != nil {
		return "", "", err
	}

	return string(specBytes), version, nil
}

func (c *client) ExecStatements(ctx context.Context, statements []string) error {
	return c.withDB(func(db *stdsql.DB) error { return sql.StdSQLExecStatements(ctx, db, statements) })
}

func (c *client) InstallFence(ctx context.Context, checkpoints sql.Table, fence sql.Fence) (sql.Fence, error) {
	var err = c.withDB(func(db *stdsql.DB) error {
		var err error
		fence, err = sql.StdInstallFence(ctx, db, checkpoints, fence, func(fenceHex string) ([]byte, error) {
			fenceHexBytes, err := hex.DecodeString(fenceHex)
			if err != nil {
				return nil, err
			}

			return base64.StdEncoding.DecodeString(string(fenceHexBytes))
		})
		return err
	})
	return fence, err
}

func (c *client) Close() {
	c.db.Close()
}

func (c *client) withDB(fn func(*stdsql.DB) error) error {
	var db, err = stdsql.Open("pgx", c.cfg.toURI())
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(db)
}
