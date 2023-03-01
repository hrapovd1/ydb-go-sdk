//go:build !fast
// +build !fast

package integration

import (
	"context"
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	"github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xtest"
	"github.com/ydb-platform/ydb-go-sdk/v3/log"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/types"
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"
)

func TestLongStream(t *testing.T) {
	t.Parallel()

	var (
		folder            = t.Name()
		tableName         = `long_stream_query`
		discoveryInterval = 10 * time.Second
		db                *ydb.Driver
		err               error
		upsertRowsCount   = 100000
		batchSize         = 10000
	)

	var (
		ctx    = xtest.Context(t)
		logger = xtest.Logger(t)
	)

	db, err = ydb.Open(
		ctx,
		os.Getenv("YDB_CONNECTION_STRING"),
		ydb.WithAccessTokenCredentials(
			os.Getenv("YDB_ACCESS_TOKEN_CREDENTIALS"),
		),
		ydb.WithDiscoveryInterval(0), // disable re-discovery on upsert time
		ydb.WithLogger(
			trace.MatchDetails(`ydb\.(driver|discovery|retry|scheme).*`),
			ydb.WithNamespace("ydb"),
			ydb.WithOutWriter(logger.Out()),
			ydb.WithErrWriter(logger.Err()),
			ydb.WithMinLevel(log.TRACE),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func(db *ydb.Driver) {
		// cleanup
		_ = db.Close(ctx)
	}(db)

	t.Run("creating", func(t *testing.T) {
		t.Run("stream", func(t *testing.T) {
			t.Run("table", func(t *testing.T) {
				if err = db.Table().Do(ctx,
					func(ctx context.Context, s table.Session) (err error) {
						_, err = s.DescribeTable(ctx, path.Join(db.Name(), folder, tableName))
						if err == nil {
							if err = s.DropTable(ctx, path.Join(db.Name(), folder, tableName)); err != nil {
								return err
							}
						}
						return s.ExecuteSchemeQuery(
							ctx,
							"CREATE TABLE `"+path.Join(db.Name(), folder, tableName)+"` (val Int64, PRIMARY KEY (val))",
						)
					},
					table.WithIdempotent(),
				); err != nil {
					t.Fatalf("create table failed: %v\n", err)
				}
			})
		})
	})

	t.Run("check", func(t *testing.T) {
		t.Run("batch", func(t *testing.T) {
			t.Run("size", func(t *testing.T) {
				if upsertRowsCount%batchSize != 0 {
					t.Fatalf("wrong batch size: (%d mod %d = %d) != 0", upsertRowsCount, batchSize, upsertRowsCount%batchSize)
				}
			})
		})
	})

	t.Run("upserting", func(t *testing.T) {
		t.Run("rows", func(t *testing.T) {
			var upserted uint32
			for i := 0; i < (upsertRowsCount / batchSize); i++ {
				var (
					from = int32(i * batchSize)
					to   = int32((i + 1) * batchSize)
				)
				t.Run(fmt.Sprintf("upserting %d..%d", from, to-1), func(t *testing.T) {
					values := make([]types.Value, 0, batchSize)
					for j := from; j < to; j++ {
						values = append(
							values,
							types.StructValue(
								types.StructFieldValue("val", types.Int32Value(j)),
							),
						)
					}
					if err = db.Table().Do(ctx,
						func(ctx context.Context, s table.Session) (err error) {
							_, _, err = s.Execute(
								ctx,
								table.TxControl(
									table.BeginTx(
										table.WithSerializableReadWrite(),
									),
									table.CommitTx(),
								), `
								DECLARE $values AS List<Struct<
									val: Int32,
								>>;
								UPSERT INTO `+"`"+path.Join(db.Name(), folder, tableName)+"`"+`
								SELECT
									val 
								FROM
									AS_TABLE($values);            
							`, table.NewQueryParameters(
									table.ValueParam(
										"$values",
										types.ListValue(values...),
									),
								),
							)
							return err
						},
						table.WithIdempotent(),
					); err != nil {
						t.Fatalf("upsert failed: %v\n", err)
					} else {
						upserted += uint32(to - from)
					}
				})
			}
			t.Run("check", func(t *testing.T) {
				t.Run("upserted", func(t *testing.T) {
					t.Run("rows", func(t *testing.T) {
						if upserted != uint32(upsertRowsCount) {
							t.Fatalf("wrong rows count: %v, expected: %d", upserted, upsertRowsCount)
						}
					})
				})
			})
		})
	})

	t.Run("make", func(t *testing.T) {
		t.Run("child", func(t *testing.T) {
			t.Run("discovered", func(t *testing.T) {
				t.Run("connection", func(t *testing.T) {
					db, err = db.With(ctx, ydb.WithDiscoveryInterval(discoveryInterval))
					if err != nil {
						t.Fatal(err)
					}
				})
			})
		})
	})

	defer func(db *ydb.Driver) {
		// cleanup
		_ = db.Close(ctx)
	}(db)

	t.Run("execute", func(t *testing.T) {
		t.Run("stream", func(t *testing.T) {
			t.Run("query", func(t *testing.T) {
				if err = db.Table().Do(ctx,
					func(ctx context.Context, s table.Session) (err error) {
						var (
							start     = time.Now()
							rowsCount = 0
						)
						res, err := s.StreamExecuteScanQuery(ctx,
							"SELECT val FROM `"+path.Join(db.Name(), folder, tableName)+"`;", nil,
						)
						if err != nil {
							return err
						}
						defer func() {
							_ = res.Close()
						}()
						for res.NextResultSet(ctx) {
							count := 0
							for res.NextRow() {
								count++
							}
							rowsCount += count
							time.Sleep(discoveryInterval)
						}
						if err = res.Err(); err != nil {
							return fmt.Errorf("received error (duration: %v): %w", time.Since(start), err)
						}
						if rowsCount != upsertRowsCount {
							return fmt.Errorf("wrong rows count: %v, expected: %d (duration: %v)",
								rowsCount,
								upsertRowsCount,
								time.Since(start),
							)
						}
						return res.Err()
					},
					table.WithIdempotent(),
				); err != nil {
					t.Fatalf("stream query failed: %v\n", err)
				}
			})
		})
	})

	t.Run("stream", func(t *testing.T) {
		t.Run("read", func(t *testing.T) {
			t.Run("table", func(t *testing.T) {
				if err = db.Table().Do(ctx,
					func(ctx context.Context, s table.Session) (err error) {
						var (
							start     = time.Now()
							rowsCount = 0
						)
						res, err := s.StreamReadTable(ctx, path.Join(db.Name(), folder, tableName), options.ReadColumn("val"))
						if err != nil {
							return err
						}
						defer func() {
							_ = res.Close()
						}()
						for res.NextResultSet(ctx) {
							count := 0
							for res.NextRow() {
								count++
							}
							rowsCount += count
							time.Sleep(discoveryInterval)
						}
						if err = res.Err(); err != nil {
							return fmt.Errorf("received error (duration: %v): %w", time.Since(start), err)
						}
						if rowsCount != upsertRowsCount {
							return fmt.Errorf("wrong rows count: %v, expected: %d (duration: %v)",
								rowsCount,
								upsertRowsCount,
								time.Since(start),
							)
						}
						return res.Err()
					},
					table.WithIdempotent(),
				); err != nil {
					t.Fatalf("stream query failed: %v\n", err)
				}
			})
		})
	})
}
