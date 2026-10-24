// openalbert-migrate performs explicit deployment maintenance while the bot is stopped.
// Secrets are read from the environment, never command-line arguments or output.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"openalbert/internal/blob"
	"openalbert/internal/domain"
	"openalbert/internal/maintenance"
	"openalbert/internal/storage"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.temporal.io/sdk/client"
)

func main() {
	mode := flag.String("mode", "", "postgres, s3, verify, replay or handoff")
	source := flag.String("source", "", "Previous database/role, metadata prefix or Temporal queue")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var err error
	switch *mode {
	case "postgres":
		err = postgres(ctx, *source)
	case "s3":
		err = migrateS3(ctx, *source)
	case "verify":
		err = verify(ctx)
	case "replay":
		if len(flag.Args()) == 0 {
			err = errors.New("history JSON files are required")
		}
		for _, path := range flag.Args() {
			if err != nil {
				break
			}
			err = maintenance.ReplayFile(path)
		}
	case "handoff":
		namespace := os.Getenv("TEMPORAL_NAMESPACE")
		if namespace == "" {
			namespace = "default"
		}
		var c client.Client
		c, err = client.Dial(client.Options{HostPort: os.Getenv("TEMPORAL_ADDRESS"), Namespace: namespace})
		if err == nil {
			defer c.Close()
			err = maintenance.HandoffQueue(ctx, c, namespace, *source, os.Getenv("TEMPORAL_TASK_QUEUE"))
		}
	default:
		err = errors.New("choose --mode postgres, s3, verify, replay or handoff")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

func postgres(ctx context.Context, source string) error {
	if source == "" || source == "openalbert" {
		return errors.New("a distinct source database/role is required")
	}
	config, err := pgx.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	config.Database = "postgres"
	c, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	var hash string
	if err = c.QueryRow(ctx, "SELECT rolpassword FROM pg_authid WHERE rolname=$1", source).Scan(&hash); err != nil {
		return err
	}
	if !strings.HasPrefix(hash, "SCRAM-SHA-256$") {
		return errors.New("role rename requires a SCRAM password; convert password before migration")
	}
	var exists bool
	if err = c.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_database WHERE datname='openalbert') OR EXISTS(SELECT FROM pg_roles WHERE rolname='openalbert')").Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("target database or role already exists")
	}
	secret := make([]byte, 24)
	if _, err = rand.Read(secret); err != nil {
		return err
	}
	admin := "openalbert_migration_" + hex.EncodeToString(secret[:6])
	password := hex.EncodeToString(secret)
	// Server-side format quotes the generated temporary password, with no SQL echo.
	var create string
	if err = c.QueryRow(ctx, "SELECT format('CREATE ROLE %I LOGIN SUPERUSER PASSWORD %L', $1::text, $2::text)", admin, password).Scan(&create); err != nil {
		return err
	}
	if _, err = c.Exec(ctx, create); err != nil {
		return err
	}
	defer func() { _, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident(admin)) }()
	adminConfig := config.Copy()
	adminConfig.User, adminConfig.Password = admin, password
	a, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		return err
	}
	defer a.Close(ctx)
	tx, err := a.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "ALTER DATABASE "+ident(source)+" RENAME TO openalbert"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "ALTER ROLE "+ident(source)+" RENAME TO openalbert"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	config.User, config.Database = "openalbert", "openalbert"
	n, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}
	defer n.Close(ctx)
	if _, err = n.Exec(ctx, "DROP ROLE "+ident(admin)); err != nil {
		return err
	}
	tx, err = n.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT relname,relkind::text FROM pg_class WHERE relnamespace='public'::regnamespace AND starts_with(relname,$1) ORDER BY relkind,relname", source+"_")
	if err != nil {
		return err
	}
	type relation struct{ name, kind string }
	var relations []relation
	for rows.Next() {
		var r relation
		if err = rows.Scan(&r.name, &r.kind); err != nil {
			rows.Close()
			return err
		}
		relations = append(relations, r)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, r := range relations {
		kind := map[string]string{"r": "TABLE", "p": "TABLE", "i": "INDEX", "S": "SEQUENCE"}[r.kind]
		if kind == "" {
			return fmt.Errorf("unsupported relation kind %s", r.kind)
		}
		if _, err = tx.Exec(ctx, "ALTER "+kind+" public."+ident(r.name)+" RENAME TO "+ident("openalbert"+strings.TrimPrefix(r.name, source))); err != nil {
			return err
		}
	}
	rows, err = tx.Query(ctx, "SELECT c.relname,k.conname FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid WHERE k.connamespace='public'::regnamespace AND starts_with(k.conname,$1)", source+"_")
	if err != nil {
		return err
	}
	var constraints [][2]string
	for rows.Next() {
		var value [2]string
		if err = rows.Scan(&value[0], &value[1]); err != nil {
			rows.Close()
			return err
		}
		constraints = append(constraints, value)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, value := range constraints {
		if _, err = tx.Exec(ctx, "ALTER TABLE public."+ident(value[0])+" RENAME CONSTRAINT "+ident(value[1])+" TO "+ident("openalbert"+strings.TrimPrefix(value[1], source))); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("Renamed database, role and %d relations; ownership and row data preserved\n", len(relations))
	return nil
}

func s3Client(prefix string) (*minio.Client, error) {
	get := func(key string) string {
		if x := os.Getenv(prefix + key); x != "" {
			return x
		}
		return os.Getenv(key)
	}
	return minio.New(get("S3_ENDPOINT"), &minio.Options{Creds: credentials.NewStaticV4(get("S3_ACCESS_KEY"), get("S3_SECRET_KEY"), ""), Secure: get("S3_SECURE") == "true"})
}

func migrateS3(ctx context.Context, sourcePrefix string) error {
	if sourcePrefix == "" {
		return errors.New("source metadata prefix is required")
	}
	source, err := s3Client("SOURCE_")
	if err != nil {
		return err
	}
	target, err := s3Client("")
	if err != nil {
		return err
	}
	from, to := os.Getenv("SOURCE_S3_BUCKET"), os.Getenv("S3_BUCKET")
	if from == "" || to == "" || from == to {
		return errors.New("distinct source and target buckets required")
	}
	versioning, err := source.GetBucketVersioning(ctx, from)
	if err != nil {
		return err
	}
	if versioning.Status != "" {
		return errors.New("versioned source buckets require a separate full-version migration")
	}
	exists, err := target.BucketExists(ctx, to)
	if err != nil {
		return err
	}
	if !exists {
		if err = target.MakeBucket(ctx, to, minio.MakeBucketOptions{}); err != nil {
			return err
		}
	}
	count := 0
	for item := range source.ListObjects(ctx, from, minio.ListObjectsOptions{Recursive: true}) {
		if item.Err != nil {
			return item.Err
		}
		object, err := source.GetObject(ctx, from, item.Key, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		info, err := object.Stat()
		if err != nil {
			object.Close()
			return err
		}
		data, err := io.ReadAll(io.LimitReader(object, 20<<20+1))
		object.Close()
		if err != nil {
			return err
		}
		if len(data) > 20<<20 {
			return errors.New("source artifact exceeds size limit")
		}
		metadata := map[string]string{}
		var encoded, expected string
		for k, v := range info.UserMetadata {
			switch {
			case strings.EqualFold(k, sourcePrefix+"-Artifact"):
				encoded = v
				metadata["OpenAlbert-Artifact"] = v
			case strings.EqualFold(k, sourcePrefix+"-Sha256"):
				expected = v
				metadata["OpenAlbert-Sha256"] = v
			default:
				metadata[k] = v
			}
		}
		var art domain.Artifact
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &art); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if art.ID == "" || art.ConversationID == "" || expected != hex.EncodeToString(sum[:]) {
			return errors.New("invalid source artifact integrity metadata")
		}
		_, err = target.PutObject(ctx, to, item.Key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: info.ContentType, UserMetadata: metadata, DisableMultipart: true})
		if err != nil {
			return err
		}
		copied, err := target.GetObject(ctx, to, item.Key, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		got, err := io.ReadAll(io.LimitReader(copied, int64(len(data))+1))
		copied.Close()
		if err != nil {
			return err
		}
		if !bytes.Equal(got, data) {
			return errors.New("copied artifact differs from source")
		}
		count++
	}
	fmt.Printf("Migrated and verified %d S3 objects; source bucket preserved\n", count)
	return nil
}

func verify(ctx context.Context) error {
	repo, err := storage.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer repo.Close()
	store, err := blob.New(ctx, repo, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), os.Getenv("S3_BUCKET"), os.Getenv("S3_SECURE") == "true")
	if err != nil {
		return err
	}
	c, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	rows, err := c.Query(ctx, "SELECT conversation_id,id FROM openalbert_artifacts ORDER BY conversation_id,id")
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var scope, id string
		if err = rows.Scan(&scope, &id); err != nil {
			return err
		}
		if _, ok, err := store.Artifact(ctx, scope, id); err != nil {
			return err
		} else if !ok {
			return errors.New("missing artifact")
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	fmt.Printf("Verified all %d database artifacts through the application S3 reader\n", count)
	return nil
}
