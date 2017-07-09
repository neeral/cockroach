// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)
// Author: Bram Gruneir (bram+code@cockroachlabs.com)
// Author: Cuong Do (cdo@cockroachlabs.com)

package server

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gogo/protobuf/proto"
	gwruntime "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"encoding/json"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/server/status"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/mon"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
)

const (
	// adminPrefix is the prefix for RESTful endpoints used to provide an
	// administrative interface to the cockroach cluster.
	adminPrefix = "/_admin/v1/"

	// defaultAPIEventLimit is the default maximum number of events returned by any
	// endpoints returning events.
	defaultAPIEventLimit = 1000
)

// apiServerMessage is the standard body for all HTTP 500 responses.
var errAdminAPIError = grpc.Errorf(codes.Internal, "An internal server error "+
	"has occurred. Please check your CockroachDB logs for more details.")

// A adminServer provides a RESTful HTTP API to administration of
// the cockroach cluster.
type adminServer struct {
	server     *Server
	memMonitor mon.MemoryMonitor
	memMetrics *sql.MemoryMetrics
}

// noteworthyAdminMemoryUsageBytes is the minimum size tracked by the
// admin SQL pool before the pool start explicitly logging overall
// usage growth in the log.
var noteworthyAdminMemoryUsageBytes = envutil.EnvOrDefaultInt64("COCKROACH_NOTEWORTHY_ADMIN_MEMORY_USAGE", 100*1024)

// newAdminServer allocates and returns a new REST server for
// administrative APIs.
func newAdminServer(s *Server) *adminServer {
	server := &adminServer{server: s, memMetrics: &s.adminMemMetrics}
	// TODO(knz): We do not limit memory usage by admin operations
	// yet. Is this wise?
	server.memMonitor = mon.MakeUnlimitedMonitor(
		context.Background(), "admin", nil, nil, noteworthyAdminMemoryUsageBytes,
	)
	return server
}

// RegisterService registers the GRPC service.
func (s *adminServer) RegisterService(g *grpc.Server) {
	serverpb.RegisterAdminServer(g, s)
}

// RegisterGateway starts the gateway (i.e. reverse proxy) that proxies HTTP requests
// to the appropriate gRPC endpoints.
func (s *adminServer) RegisterGateway(
	ctx context.Context, mux *gwruntime.ServeMux, conn *grpc.ClientConn,
) error {
	return serverpb.RegisterAdminHandler(ctx, mux, conn)
}

// getUserProto will return the authenticated user. For now, this is just a stub until we
// figure out our authentication mechanism.
//
// TODO(cdo): Make this work when we have an authentication scheme for the
// API.
func (s *adminServer) getUser(_ proto.Message) string {
	return security.RootUser
}

// serverError logs the provided error and returns an error that should be returned by
// the RPC endpoint method.
func (s *adminServer) serverError(err error) error {
	log.ErrorfDepth(context.TODO(), 1, "%s", err)
	return errAdminAPIError
}

// serverErrorf logs the provided error and returns an error that should be returned by
// the RPC endpoint method.
func (s *adminServer) serverErrorf(format string, args ...interface{}) error {
	log.ErrorfDepth(context.TODO(), 1, format, args...)
	return errAdminAPIError
}

// serverErrors logs the provided errors and returns an error that should be returned by
// the RPC endpoint method.
func (s *adminServer) serverErrors(errors []error) error {
	log.ErrorfDepth(context.TODO(), 1, "%v", errors)
	return errAdminAPIError
}

// checkQueryResults performs basic tests on the provided query results and returns
// the first error that was found.
func (s *adminServer) checkQueryResults(results []sql.Result, numResults int) error {
	if a, e := len(results), numResults; a != e {
		return errors.Errorf("# of results %d != expected %d", a, e)
	}

	for _, result := range results {
		if result.Err != nil {
			return errors.Errorf("%s", result.Err)
		}
	}

	return nil
}

// firstNotFoundError returns the first table/database not found error in the
// provided results.
func (s *adminServer) firstNotFoundError(results []sql.Result) error {
	for _, res := range results {
		// TODO(cdo): Replace this crude suffix-matching with something more structured once we have
		// more structured errors.
		if res.Err != nil && strings.HasSuffix(res.Err.Error(), "does not exist") {
			return res.Err
		}
	}

	return nil
}

// NewContextAndSessionForRPC creates a context and SQL session to be used for
// serving an RPC request.
// The session will be initialized with a context derived from the returned one.
func (s *adminServer) NewContextAndSessionForRPC(
	ctx context.Context, args sql.SessionArgs,
) (context.Context, *sql.Session) {
	ctx = s.server.AnnotateCtx(ctx)
	session := sql.NewSession(ctx, args, s.server.sqlExecutor, nil, s.memMetrics)
	session.StartMonitor(&s.memMonitor, mon.BoundAccount{})
	return ctx, session
}

// Databases is an endpoint that returns a list of databases.
func (s *adminServer) Databases(
	ctx context.Context, req *serverpb.DatabasesRequest,
) (*serverpb.DatabasesResponse, error) {
	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)
	r := s.server.sqlExecutor.ExecuteStatements(session, "SHOW DATABASES;", nil)
	defer r.Close(ctx)
	if err := s.checkQueryResults(r.ResultList, 1); err != nil {
		return nil, s.serverError(err)
	}

	var resp serverpb.DatabasesResponse
	for i, nRows := 0, r.ResultList[0].Rows.Len(); i < nRows; i++ {
		row := r.ResultList[0].Rows.At(i)
		dbDatum, ok := parser.AsDString(row[0])
		if !ok {
			return nil, s.serverErrorf("type assertion failed on db name: %T", row[0])
		}
		dbName := string(dbDatum)
		if !s.server.sqlExecutor.IsVirtualDatabase(dbName) {
			resp.Databases = append(resp.Databases, dbName)
		}
	}

	return &resp, nil
}

// DatabaseDetails is an endpoint that returns grants and a list of table names
// for the specified database.
func (s *adminServer) DatabaseDetails(
	ctx context.Context, req *serverpb.DatabaseDetailsRequest,
) (*serverpb.DatabaseDetailsResponse, error) {
	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)

	escDBName := parser.Name(req.Database).String()
	if err := s.assertNotVirtualSchema(escDBName); err != nil {
		return nil, err
	}

	// Placeholders don't work with SHOW statements, so we need to manually
	// escape the database name.
	//
	// TODO(cdo): Use placeholders when they're supported by SHOW.
	query := fmt.Sprintf("SHOW GRANTS ON DATABASE %s; SHOW TABLES FROM %s;", escDBName, escDBName)
	r := s.server.sqlExecutor.ExecuteStatements(session, query, nil)
	defer r.Close(ctx)
	if err := s.firstNotFoundError(r.ResultList); err != nil {
		return nil, grpc.Errorf(codes.NotFound, "%s", err)
	}
	if err := s.checkQueryResults(r.ResultList, 2); err != nil {
		return nil, s.serverError(err)
	}

	// Marshal grants.
	var resp serverpb.DatabaseDetailsResponse
	{
		const (
			userCol       = "User"
			privilegesCol = "Privileges"
		)

		scanner := makeResultScanner(r.ResultList[0].Columns)
		for i, nRows := 0, r.ResultList[0].Rows.Len(); i < nRows; i++ {
			row := r.ResultList[0].Rows.At(i)
			// Marshal grant, splitting comma-separated privileges into a proper slice.
			var grant serverpb.DatabaseDetailsResponse_Grant
			var privileges string
			if err := scanner.Scan(row, userCol, &grant.User); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, privilegesCol, &privileges); err != nil {
				return nil, err
			}
			grant.Privileges = strings.Split(privileges, ",")
			resp.Grants = append(resp.Grants, grant)
		}
	}

	// Marshal table names.
	{
		const tableCol = "Table"
		scanner := makeResultScanner(r.ResultList[1].Columns)
		if a, e := len(r.ResultList[1].Columns), 1; a != e {
			return nil, s.serverErrorf("show tables columns mismatch: %d != expected %d", a, e)
		}
		for i, nRows := 0, r.ResultList[1].Rows.Len(); i < nRows; i++ {
			row := r.ResultList[1].Rows.At(i)
			var tableName string
			if err := scanner.Scan(row, tableCol, &tableName); err != nil {
				return nil, err
			}
			resp.TableNames = append(resp.TableNames, tableName)
		}
	}

	// Query the descriptor ID and zone configuration for this database.
	{
		path, err := s.queryDescriptorIDPath(ctx, session, []string{req.Database})
		if err != nil {
			return nil, s.serverError(err)
		}
		resp.DescriptorID = int64(path[1])

		id, zone, zoneExists, err := s.queryZonePath(ctx, session, path)
		if err != nil {
			return nil, s.serverError(err)
		}

		if !zoneExists {
			zone = config.DefaultZoneConfig()
		}
		resp.ZoneConfig = zone

		switch id {
		case path[1]:
			resp.ZoneConfigLevel = serverpb.ZoneConfigurationLevel_DATABASE
		default:
			resp.ZoneConfigLevel = serverpb.ZoneConfigurationLevel_CLUSTER
		}
	}

	return &resp, nil
}

// TableDetails is an endpoint that returns columns, indices, and other
// relevant details for the specified table.
func (s *adminServer) TableDetails(
	ctx context.Context, req *serverpb.TableDetailsRequest,
) (*serverpb.TableDetailsResponse, error) {
	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)

	escDBName := parser.Name(req.Database).String()
	if err := s.assertNotVirtualSchema(escDBName); err != nil {
		return nil, err
	}

	// TODO(cdo): Use real placeholders for the table and database names when we've extended our SQL
	// grammar to allow that.
	escTableName := parser.Name(req.Table).String()
	escQualTable := fmt.Sprintf("%s.%s", escDBName, escTableName)
	query := fmt.Sprintf("SHOW COLUMNS FROM %[1]s; SHOW INDEX FROM %[1]s; SHOW GRANTS ON TABLE %[1]s; SHOW CREATE TABLE %[1]s;",
		escQualTable)
	r := s.server.sqlExecutor.ExecuteStatements(session, query, nil)
	defer r.Close(ctx)
	if err := s.firstNotFoundError(r.ResultList); err != nil {
		return nil, grpc.Errorf(codes.NotFound, "%s", err)
	}
	if err := s.checkQueryResults(r.ResultList, 4); err != nil {
		return nil, err
	}

	var resp serverpb.TableDetailsResponse

	// Marshal SHOW COLUMNS result.
	//
	// TODO(cdo): protobuf v3's default behavior for fields with zero values (e.g. empty strings)
	// is to suppress them. So, if protobuf field "foo" is an empty string, "foo" won't show
	// up in the marshalled JSON. I feel that this is counterintuitive, and this should be fixed
	// for our API.
	{
		const (
			fieldCol   = "Field" // column name
			typeCol    = "Type"
			nullCol    = "Null"
			defaultCol = "Default"
		)
		scanner := makeResultScanner(r.ResultList[0].Columns)
		for i, nRows := 0, r.ResultList[0].Rows.Len(); i < nRows; i++ {
			row := r.ResultList[0].Rows.At(i)
			var col serverpb.TableDetailsResponse_Column
			if err := scanner.Scan(row, fieldCol, &col.Name); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, typeCol, &col.Type); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, nullCol, &col.Nullable); err != nil {
				return nil, err
			}
			isDefaultNull, err := scanner.IsNull(row, defaultCol)
			if err != nil {
				return nil, err
			}
			if !isDefaultNull {
				if err := scanner.Scan(row, defaultCol, &col.DefaultValue); err != nil {
					return nil, err
				}
			}
			resp.Columns = append(resp.Columns, col)
		}
	}

	// Marshal SHOW INDEX result.
	{
		const (
			nameCol      = "Name"
			uniqueCol    = "Unique"
			seqCol       = "Seq"
			columnCol    = "Column"
			directionCol = "Direction"
			storingCol   = "Storing"
			implicitCol  = "Implicit"
		)
		scanner := makeResultScanner(r.ResultList[1].Columns)
		for i, nRows := 0, r.ResultList[1].Rows.Len(); i < nRows; i++ {
			row := r.ResultList[1].Rows.At(i)
			// Marshal grant, splitting comma-separated privileges into a proper slice.
			var index serverpb.TableDetailsResponse_Index
			if err := scanner.Scan(row, nameCol, &index.Name); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, uniqueCol, &index.Unique); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, seqCol, &index.Seq); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, columnCol, &index.Column); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, directionCol, &index.Direction); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, storingCol, &index.Storing); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, implicitCol, &index.Implicit); err != nil {
				return nil, err
			}
			resp.Indexes = append(resp.Indexes, index)
		}
	}

	// Marshal SHOW GRANTS result.
	{
		const (
			userCol       = "User"
			privilegesCol = "Privileges"
		)
		scanner := makeResultScanner(r.ResultList[2].Columns)
		for i, nRows := 0, r.ResultList[2].Rows.Len(); i < nRows; i++ {
			row := r.ResultList[2].Rows.At(i)
			// Marshal grant, splitting comma-separated privileges into a proper slice.
			var grant serverpb.TableDetailsResponse_Grant
			var privileges string
			if err := scanner.Scan(row, userCol, &grant.User); err != nil {
				return nil, err
			}
			if err := scanner.Scan(row, privilegesCol, &privileges); err != nil {
				return nil, err
			}
			grant.Privileges = strings.Split(privileges, ",")
			resp.Grants = append(resp.Grants, grant)
		}
	}

	// Marshal SHOW CREATE TABLE result.
	{
		const createTableCol = "CreateTable"
		showResult := r.ResultList[3]
		if showResult.Rows.Len() != 1 {
			return nil, s.serverErrorf("CreateTable response not available.")
		}

		scanner := makeResultScanner(showResult.Columns)
		var createStmt string
		if err := scanner.Scan(showResult.Rows.At(0), createTableCol, &createStmt); err != nil {
			return nil, err
		}

		resp.CreateTableStatement = createStmt
	}

	// Get the number of ranges in the table. We get the key span for the table
	// data. Then, we count the number of ranges that make up that key span.
	{
		iexecutor := sql.InternalExecutor{LeaseManager: s.server.leaseMgr}
		var tableSpan roachpb.Span
		if err := s.server.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
			var err error
			tableSpan, err = iexecutor.GetTableSpan(
				ctx, s.getUser(req), txn, req.Database, req.Table,
			)
			return err
		}); err != nil {
			return nil, s.serverError(err)
		}
		tableRSpan := roachpb.RSpan{}
		var err error
		tableRSpan.Key, err = keys.Addr(tableSpan.Key)
		if err != nil {
			return nil, s.serverError(err)
		}
		tableRSpan.EndKey, err = keys.Addr(tableSpan.EndKey)
		if err != nil {
			return nil, s.serverError(err)
		}
		rangeCount, err := s.server.distSender.CountRanges(ctx, tableRSpan)
		if err != nil {
			return nil, s.serverError(err)
		}
		resp.RangeCount = rangeCount
	}

	// Query the descriptor ID and zone configuration for this table.
	{
		path, err := s.queryDescriptorIDPath(ctx, session, []string{req.Database, req.Table})
		if err != nil {
			return nil, s.serverError(err)
		}
		resp.DescriptorID = int64(path[2])

		id, zone, zoneExists, err := s.queryZonePath(ctx, session, path)
		if err != nil {
			return nil, s.serverError(err)
		}

		if !zoneExists {
			zone = config.DefaultZoneConfig()
		}
		resp.ZoneConfig = zone

		switch id {
		case path[1]:
			resp.ZoneConfigLevel = serverpb.ZoneConfigurationLevel_DATABASE
		case path[2]:
			resp.ZoneConfigLevel = serverpb.ZoneConfigurationLevel_TABLE
		default:
			resp.ZoneConfigLevel = serverpb.ZoneConfigurationLevel_CLUSTER
		}
	}

	return &resp, nil
}

// TableStats is an endpoint that returns columns, indices, and other
// relevant details for the specified table.
func (s *adminServer) TableStats(
	ctx context.Context, req *serverpb.TableStatsRequest,
) (*serverpb.TableStatsResponse, error) {
	escDBName := parser.Name(req.Database).String()
	if err := s.assertNotVirtualSchema(escDBName); err != nil {
		return nil, err
	}

	// Get table span.
	var tableSpan roachpb.Span
	iexecutor := sql.InternalExecutor{LeaseManager: s.server.leaseMgr}
	if err := s.server.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
		var err error
		tableSpan, err = iexecutor.GetTableSpan(ctx, s.getUser(req), txn, req.Database, req.Table)
		return err
	}); err != nil {
		return nil, s.serverError(err)
	}

	startKey, err := keys.Addr(tableSpan.Key)
	if err != nil {
		return nil, s.serverError(err)
	}
	endKey, err := keys.Addr(tableSpan.EndKey)
	if err != nil {
		return nil, s.serverError(err)
	}

	// Get current range descriptors for table. This is done by scanning over
	// meta2 keys for the range.
	rangeDescKVs, err := s.server.db.Scan(ctx, keys.RangeMetaKey(startKey), keys.RangeMetaKey(endKey), 0)
	if err != nil {
		return nil, s.serverError(err)
	}

	// Extract a list of node IDs from the response.
	nodeIDs := make(map[roachpb.NodeID]struct{})
	for _, kv := range rangeDescKVs {
		var rng roachpb.RangeDescriptor
		if err := kv.Value.GetProto(&rng); err != nil {
			return nil, s.serverError(err)
		}
		for _, repl := range rng.Replicas {
			nodeIDs[repl.NodeID] = struct{}{}
		}
	}

	// Construct TableStatsResponse by sending an RPC to every node involved.
	tableStatResponse := serverpb.TableStatsResponse{
		NodeCount: int64(len(nodeIDs)),
		// TODO(mrtracy): The "RangeCount" returned by TableStats is more
		// accurate than the "RangeCount" returned by TableDetails, because this
		// method always consistently queries the meta2 key range for the table;
		// in contrast, TableDetails uses a method on the DistSender, which
		// queries using a range metadata cache and thus may return stale data
		// for tables that are rapidly splitting. However, one potential
		// *advantage* of using the DistSender is that it will populate the
		// DistSender's range metadata cache in the case where meta2 information
		// for this table is not already present; the query used by TableStats
		// does not populate the DistSender cache. We should consider plumbing
		// TableStats' meta2 query through the DistSender so that it will share
		// the advantage of populating the cache (without the disadvantage of
		// potentially returning stale data).
		// See Github #5435 for some discussion.
		RangeCount: int64(len(rangeDescKVs)),
	}
	type nodeResponse struct {
		nodeID roachpb.NodeID
		resp   *serverpb.SpanStatsResponse
		err    error
	}

	// Send a SpanStats query to each node. Set a timeout on the context for
	// these queries.
	responses := make(chan nodeResponse)
	nodeCtx, cancel := context.WithTimeout(ctx, base.NetworkTimeout)
	defer cancel()
	for nodeID := range nodeIDs {
		nodeID := nodeID
		if err := s.server.stopper.RunAsyncTask(
			nodeCtx, "server.adminServer: requesting remote stats",
			func(ctx context.Context) {
				var spanResponse *serverpb.SpanStatsResponse
				client, err := s.server.status.dialNode(nodeID)
				if err == nil {
					req := serverpb.SpanStatsRequest{
						StartKey: startKey,
						EndKey:   endKey,
						NodeID:   nodeID.String(),
					}
					spanResponse, err = client.SpanStats(ctx, &req)
				}

				response := nodeResponse{
					nodeID: nodeID,
					resp:   spanResponse,
					err:    err,
				}
				select {
				case responses <- response:
					// Response processed.
				case <-ctx.Done():
					// Context completed, response no longer needed.
				}
			}); err != nil {
			return nil, err
		}
	}
	for remainingResponses := len(nodeIDs); remainingResponses > 0; remainingResponses-- {
		select {
		case resp := <-responses:
			// For nodes which returned an error, note that the node's data
			// is missing. For successful calls, aggregate statistics.
			if resp.err != nil {
				tableStatResponse.MissingNodes = append(
					tableStatResponse.MissingNodes,
					serverpb.TableStatsResponse_MissingNode{
						NodeID:       resp.nodeID.String(),
						ErrorMessage: resp.err.Error(),
					},
				)
			} else {
				tableStatResponse.Stats.Add(resp.resp.TotalStats)
				tableStatResponse.ReplicaCount += int64(resp.resp.RangeCount)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return &tableStatResponse, nil
}

// Users returns a list of users, stripped of any passwords.
func (s *adminServer) Users(
	ctx context.Context, req *serverpb.UsersRequest,
) (*serverpb.UsersResponse, error) {
	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)
	query := "SELECT username FROM system.users"
	r := s.server.sqlExecutor.ExecuteStatements(session, query, nil)
	defer r.Close(ctx)
	if err := s.checkQueryResults(r.ResultList, 1); err != nil {
		return nil, s.serverError(err)
	}

	var resp serverpb.UsersResponse
	for i, nRows := 0, r.ResultList[0].Rows.Len(); i < nRows; i++ {
		row := r.ResultList[0].Rows.At(i)
		resp.Users = append(resp.Users, serverpb.UsersResponse_User{Username: string(parser.MustBeDString(row[0]))})
	}
	return &resp, nil
}

// Events is an endpoint that returns the latest event log entries, with the following
// optional URL parameters:
//
// type=STRING  returns events with this type (e.g. "create_table")
// targetID=INT returns events for that have this targetID
func (s *adminServer) Events(
	ctx context.Context, req *serverpb.EventsRequest,
) (*serverpb.EventsResponse, error) {
	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)

	limit := req.Limit
	if limit == 0 {
		limit = defaultAPIEventLimit
	}

	// Execute the query.
	q := makeSQLQuery()
	q.Append(`SELECT timestamp, "eventType", "targetID", "reportingID", info, "uniqueID" `)
	q.Append("FROM system.eventlog ")
	q.Append("WHERE true ") // This simplifies the WHERE clause logic below.
	if len(req.Type) > 0 {
		q.Append(`AND "eventType" = $ `, parser.NewDString(req.Type))
	}
	if req.TargetId > 0 {
		q.Append(`AND "targetID" = $ `, parser.NewDInt(parser.DInt(req.TargetId)))
	}
	q.Append("ORDER BY timestamp DESC ")
	if limit > 0 {
		q.Append("LIMIT $", parser.NewDInt(parser.DInt(limit)))
	}
	if len(q.Errors()) > 0 {
		return nil, s.serverErrors(q.Errors())
	}
	r := s.server.sqlExecutor.ExecuteStatements(session, q.String(), q.QueryArguments())
	defer r.Close(ctx)
	if err := s.checkQueryResults(r.ResultList, 1); err != nil {
		return nil, s.serverError(err)
	}

	// Marshal response.
	var resp serverpb.EventsResponse
	scanner := makeResultScanner(r.ResultList[0].Columns)
	for i, nRows := 0, r.ResultList[0].Rows.Len(); i < nRows; i++ {
		row := r.ResultList[0].Rows.At(i)
		var event serverpb.EventsResponse_Event
		var ts time.Time
		if err := scanner.ScanIndex(row, 0, &ts); err != nil {
			return nil, err
		}
		event.Timestamp = ts
		if err := scanner.ScanIndex(row, 1, &event.EventType); err != nil {
			return nil, err
		}
		if err := scanner.ScanIndex(row, 2, &event.TargetID); err != nil {
			return nil, err
		}
		if err := scanner.ScanIndex(row, 3, &event.ReportingID); err != nil {
			return nil, err
		}
		if err := scanner.ScanIndex(row, 4, &event.Info); err != nil {
			return nil, err
		}
		if err := scanner.ScanIndex(row, 5, &event.UniqueID); err != nil {
			return nil, err
		}

		resp.Events = append(resp.Events, event)
	}
	return &resp, nil
}

// RangeLog is an endpoint that returns the latest range log entries.
func (s *adminServer) RangeLog(
	ctx context.Context, req *serverpb.RangeLogRequest,
) (*serverpb.RangeLogResponse, error) {
	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)

	limit := req.Limit
	if limit == 0 {
		limit = defaultAPIEventLimit
	}

	// Execute the query.
	q := makeSQLQuery()
	q.Append(`SELECT timestamp, "rangeID", "storeID", "eventType", "otherRangeID", info `)
	q.Append("FROM system.rangelog ")
	rangeID := parser.NewDInt(parser.DInt(req.RangeId))
	q.Append(`WHERE "rangeID" = $ OR "otherRangeID" = $`, rangeID, rangeID)
	q.Append("ORDER BY timestamp DESC ")
	if limit > 0 {
		q.Append("LIMIT $", parser.NewDInt(parser.DInt(limit)))
	}
	if len(q.Errors()) > 0 {
		return nil, s.serverErrors(q.Errors())
	}
	r := s.server.sqlExecutor.ExecuteStatements(session, q.String(), q.QueryArguments())
	defer r.Close(ctx)
	if err := s.checkQueryResults(r.ResultList, 1); err != nil {
		return nil, s.serverError(err)
	}

	// Marshal response.
	var resp serverpb.RangeLogResponse
	scanner := makeResultScanner(r.ResultList[0].Columns)
	for i, nRows := 0, r.ResultList[0].Rows.Len(); i < nRows; i++ {
		row := r.ResultList[0].Rows.At(i)
		if row.Len() != 6 {
			return nil, errors.Errorf("incorrect number of columns in response, expected 6, got %d", row.Len())
		}
		var event storage.RangeLogEvent
		var ts time.Time
		if err := scanner.ScanIndex(row, 0, &ts); err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("Timestamp didn't parse correctly: %s", row[0].String()))
		}
		event.Timestamp = ts
		var rangeID int64
		if err := scanner.ScanIndex(row, 1, &rangeID); err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("RangeID didn't parse correctly: %s", row[1].String()))
		}
		event.RangeID = roachpb.RangeID(rangeID)
		var storeID int64
		if err := scanner.ScanIndex(row, 2, &storeID); err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("StoreID didn't parse correctly: %s", row[2].String()))
		}
		event.StoreID = roachpb.StoreID(int32(storeID))
		var eventTypeString string
		if err := scanner.ScanIndex(row, 3, &eventTypeString); err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("EventType didn't parse correctly: %s", row[3].String()))
		}
		if eventType, ok := storage.RangeLogEventType_value[eventTypeString]; ok {
			event.EventType = storage.RangeLogEventType(eventType)
		} else {
			return nil, errors.Errorf("EventType didn't parse correctly: %s", eventTypeString)
		}

		var otherRangeID int64
		if row[4].String() != "NULL" {
			if err := scanner.ScanIndex(row, 4, &otherRangeID); err != nil {
				return nil, errors.Wrap(err, fmt.Sprintf("OtherRangeID didn't parse correctly: %s", row[4].String()))
			}
			event.OtherRangeID = roachpb.RangeID(otherRangeID)
		}
		if row[5].String() != "NULL" {
			var info string
			if err := scanner.ScanIndex(row, 5, &info); err != nil {
				return nil, errors.Wrap(err, fmt.Sprintf("info didn't parse correctly: %s", row[5].String()))
			}
			if err := json.Unmarshal([]byte(info), &event.Info); err != nil {
				return nil, errors.Wrap(err, fmt.Sprintf("info didn't parse correctly: %s", info))
			}
		}

		resp.Events = append(resp.Events, event)
	}
	return &resp, nil
}

// getUIData returns the values and timestamps for the given UI keys. Keys
// that are not found will not be returned.
func (s *adminServer) getUIData(
	ctx context.Context, session *sql.Session, user string, keys []string,
) (*serverpb.GetUIDataResponse, error) {
	if len(keys) == 0 {
		return &serverpb.GetUIDataResponse{}, nil
	}

	// Query database.
	query := makeSQLQuery()
	query.Append(`SELECT key, value, "lastUpdated" FROM system.ui WHERE key IN (`)
	for i, key := range keys {
		if i != 0 {
			query.Append(",")
		}
		query.Append("$", parser.NewDString(key))
	}
	query.Append(");")
	if err := query.Errors(); err != nil {
		return nil, s.serverErrorf("error constructing query: %v", err)
	}
	r := s.server.sqlExecutor.ExecuteStatements(session, query.String(), query.QueryArguments())
	defer r.Close(ctx)
	if err := s.checkQueryResults(r.ResultList, 1); err != nil {
		return nil, s.serverError(err)
	}

	// Marshal results.
	resp := serverpb.GetUIDataResponse{KeyValues: make(map[string]serverpb.GetUIDataResponse_Value)}
	for i, nRows := 0, r.ResultList[0].Rows.Len(); i < nRows; i++ {
		row := r.ResultList[0].Rows.At(i)
		dKey, ok := parser.AsDString(row[0])
		if !ok {
			return nil, s.serverErrorf("unexpected type for UI key: %T", row[0])
		}
		dValue, ok := row[1].(*parser.DBytes)
		if !ok {
			return nil, s.serverErrorf("unexpected type for UI value: %T", row[1])
		}
		dLastUpdated, ok := row[2].(*parser.DTimestamp)
		if !ok {
			return nil, s.serverErrorf("unexpected type for UI lastUpdated: %T", row[2])
		}

		resp.KeyValues[string(dKey)] = serverpb.GetUIDataResponse_Value{
			Value:       []byte(*dValue),
			LastUpdated: dLastUpdated.Time,
		}
	}
	return &resp, nil
}

// SetUIData is an endpoint that stores the given key/value pairs in the
// system.ui table. See GetUIData for more details on semantics.
func (s *adminServer) SetUIData(
	ctx context.Context, req *serverpb.SetUIDataRequest,
) (*serverpb.SetUIDataResponse, error) {
	if len(req.KeyValues) == 0 {
		return nil, grpc.Errorf(codes.InvalidArgument, "KeyValues cannot be empty")
	}

	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)

	for key, val := range req.KeyValues {
		// Do an upsert of the key. We update each key in a separate transaction to
		// avoid long-running transactions and possible deadlocks.
		query := `UPSERT INTO system.ui (key, value, "lastUpdated") VALUES ($1, $2, NOW())`
		qargs := parser.MakePlaceholderInfo()
		qargs.SetValue(`1`, parser.NewDString(key))
		qargs.SetValue(`2`, parser.NewDBytes(parser.DBytes(val)))
		r := s.server.sqlExecutor.ExecuteStatements(session, query, &qargs)
		defer r.Close(ctx)
		if err := s.checkQueryResults(r.ResultList, 1); err != nil {
			return nil, s.serverError(err)
		}
		if a, e := r.ResultList[0].RowsAffected, 1; a != e {
			return nil, s.serverErrorf("rows affected %d != expected %d", a, e)
		}
	}

	return &serverpb.SetUIDataResponse{}, nil
}

// GetUIData returns data associated with the given keys, which was stored
// earlier through SetUIData.
//
// The stored values are meant to be opaque to the server. In the rare case that
// the server code needs to call this method, it should only read from keys that
// have the prefix `serverUIDataKeyPrefix`.
func (s *adminServer) GetUIData(
	ctx context.Context, req *serverpb.GetUIDataRequest,
) (*serverpb.GetUIDataResponse, error) {
	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)

	if len(req.Keys) == 0 {
		return nil, grpc.Errorf(codes.InvalidArgument, "keys cannot be empty")
	}

	resp, err := s.getUIData(ctx, session, s.getUser(req), req.Keys)
	if err != nil {
		return nil, s.serverError(err)
	}

	return resp, nil
}

// Settings returns settings associated with the given keys.
func (s *adminServer) Settings(
	ctx context.Context, req *serverpb.SettingsRequest,
) (*serverpb.SettingsResponse, error) {
	keys := req.Keys
	if len(keys) == 0 {
		keys = settings.Keys()
	}

	resp := serverpb.SettingsResponse{KeyValues: make(map[string]serverpb.SettingsResponse_Value)}
	for _, k := range keys {
		v, ok := settings.Lookup(k)
		if !ok {
			continue
		}
		resp.KeyValues[k] = serverpb.SettingsResponse_Value{
			Type:        v.Typ(),
			Value:       v.String(),
			Description: v.Description(),
		}
	}

	return &resp, nil
}

// Cluster returns cluster metadata.
func (s *adminServer) Cluster(
	_ context.Context, req *serverpb.ClusterRequest,
) (*serverpb.ClusterResponse, error) {
	clusterID := s.server.node.ClusterID
	if clusterID == (uuid.UUID{}) {
		return nil, grpc.Errorf(codes.Unavailable, "cluster ID not yet available")
	}
	return &serverpb.ClusterResponse{ClusterID: clusterID.String()}, nil
}

// Health returns liveness for the node target of the request.
func (s *adminServer) Health(
	ctx context.Context, req *serverpb.HealthRequest,
) (*serverpb.HealthResponse, error) {
	isLive, err := s.server.nodeLiveness.IsLive(s.server.NodeID())
	if err != nil {
		return nil, grpc.Errorf(codes.Internal, err.Error())
	}
	if !isLive {
		return nil, grpc.Errorf(codes.Unavailable, "node is not live")
	}
	return &serverpb.HealthResponse{}, nil
}

// Liveness returns the liveness state of all nodes on the cluster.
func (s *adminServer) Liveness(
	context.Context, *serverpb.LivenessRequest,
) (*serverpb.LivenessResponse, error) {
	return &serverpb.LivenessResponse{
		Livenesses: s.server.nodeLiveness.GetLivenesses(),
	}, nil
}

// QueryPlan returns a JSON representation of a distsql physical query
// plan.
func (s *adminServer) QueryPlan(
	ctx context.Context, req *serverpb.QueryPlanRequest,
) (*serverpb.QueryPlanResponse, error) {
	args := sql.SessionArgs{User: s.getUser(req)}
	ctx, session := s.NewContextAndSessionForRPC(ctx, args)
	defer session.Finish(s.server.sqlExecutor)

	// As long as there's only one query provided it's safe to construct the
	// explain query.
	stmts, err := parser.Parse(req.Query)
	if err != nil {
		return nil, s.serverError(err)
	}
	if len(stmts) > 1 {
		return nil, s.serverErrorf("more than one query provided")
	}

	explain := fmt.Sprintf(
		"SELECT JSON FROM [EXPLAIN (distsql) %s]",
		strings.Trim(req.Query, ";"))
	r := s.server.sqlExecutor.ExecuteStatements(session, explain, nil)
	defer r.Close(ctx)
	if err := s.checkQueryResults(r.ResultList, 1); err != nil {
		return nil, s.serverError(err)
	}

	row := r.ResultList[0].Rows.At(0)
	dbDatum, ok := parser.AsDString(row[0])
	if !ok {
		return nil, s.serverErrorf("type assertion failed on json: %T", row[0])
	}

	return &serverpb.QueryPlanResponse{
		DistSQLPhysicalQueryPlan: string(dbDatum),
	}, nil
}

// Drain puts the node into the specified drain mode(s) and optionally
// instructs the process to terminate.
func (s *adminServer) Drain(req *serverpb.DrainRequest, stream serverpb.Admin_DrainServer) error {
	on := make([]serverpb.DrainMode, len(req.On))
	for i := range req.On {
		on[i] = serverpb.DrainMode(req.On[i])
	}
	off := make([]serverpb.DrainMode, len(req.Off))
	for i := range req.Off {
		off[i] = serverpb.DrainMode(req.Off[i])
	}

	_ = s.server.Undrain(off)

	nowOn, err := s.server.Drain(on)
	if err != nil {
		return err
	}

	res := serverpb.DrainResponse{
		On: make([]int32, len(nowOn)),
	}
	for i := range nowOn {
		res.On[i] = int32(nowOn[i])
	}
	if err := stream.Send(&res); err != nil {
		return err
	}

	if !req.Shutdown {
		return nil
	}

	s.server.grpc.Stop()

	ctx := stream.Context()
	go s.server.stopper.Stop(ctx)

	select {
	case <-s.server.stopper.IsStopped():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Decommission sets the decommission flag to the specified value on the specified node(s).
func (s *adminServer) Decommission(
	ctx context.Context, req *serverpb.DecommissionRequest,
) (*serverpb.DecommissionResponse, error) {
	nodeIDs := make([]roachpb.NodeID, len(req.NodeID))
	for i, sNodeID := range req.NodeID {
		if nodeID, err := strconv.Atoi(sNodeID); err == nil {
			nodeIDs[i] = roachpb.NodeID(nodeID)
		} else {
			return nil, err
		}
	}
	s.server.Decommission(req.Decommissioning, nodeIDs)
	// Get the number of replicas on each node.
	var nodeStatuses []status.NodeStatus
	if res, err := s.server.status.Nodes(ctx, &serverpb.NodesRequest{}); err != nil {
		return nil, err
	} else {
		nodeStatuses = res.Nodes
	}
	nodeReplicas := make(map[roachpb.NodeID]int64)
	for _, nodeStatus := range nodeStatuses {
		nodeID := nodeStatus.Desc.NodeID
		var replicas float64 = 0
		for _, storeStatus := range nodeStatus.StoreStatuses {
			replicas += storeStatus.Metrics["replicas"]
		}
		nodeReplicas[nodeID] = int64(replicas)
	}
	// Get node liveness.
	nl := s.server.nodeLiveness
	ls := nl.GetLivenesses()
	// Construct response.
	res := serverpb.DecommissionResponse{
		Nodes: make([]serverpb.DecommissionResponse_Value, len(ls)),
	}
	for i, l := range ls {
		res.Nodes[i] = serverpb.DecommissionResponse_Value{}
		res.Nodes[i].NodeID = int32(l.NodeID)
		if live, err := nl.IsLive(l.NodeID); err != nil {
			res.Nodes[i].IsLive = live
		} // Else default value is false
		res.Nodes[i].ReplicaCount = nodeReplicas[l.NodeID]
		res.Nodes[i].Decommissioning = l.Decommissioning
		res.Nodes[i].Draining = l.Draining
	}
	return &res, nil
}

// sqlQuery allows you to incrementally build a SQL query that uses
// placeholders. Instead of specific placeholders like $1, you instead use the
// temporary placeholder $.
type sqlQuery struct {
	buf   bytes.Buffer
	pidx  int
	qargs parser.PlaceholderInfo
	errs  []error
}

func makeSQLQuery() *sqlQuery {
	res := &sqlQuery{}
	res.qargs.Clear()
	return res
}

// String returns the full query.
func (q *sqlQuery) String() string {
	if len(q.errs) > 0 {
		return "couldn't generate query: please check Errors()"
	}
	return q.buf.String()
}

// Errors returns a slice containing all errors that have happened during the
// construction of this query.
func (q *sqlQuery) Errors() []error {
	return q.errs
}

// QueryArguments returns a filled map of placeholders containing all arguments
// provided to this query through Append.
func (q *sqlQuery) QueryArguments() *parser.PlaceholderInfo {
	return &q.qargs
}

// Append appends the provided string and any number of query parameters.
// Instead of using normal placeholders (e.g. $1, $2), use meta-placeholder $.
// This method rewrites the query so that it uses proper placeholders.
//
// For example, suppose we have the following calls:
//
//   query.Append("SELECT * FROM foo WHERE a > $ AND a < $ ", arg1, arg2)
//   query.Append("LIMIT $", limit)
//
// The query is rewritten into:
//
//   SELECT * FROM foo WHERE a > $1 AND a < $2 LIMIT $3
//   /* $1 = arg1, $2 = arg2, $3 = limit */
//
// Note that this method does NOT return any errors. Instead, we queue up
// errors, which can later be accessed. Returning an error here would make
// query construction code exceedingly tedious.
func (q *sqlQuery) Append(s string, params ...parser.Datum) {
	var placeholders int
	firstpidx := q.pidx
	for _, r := range s {
		q.buf.WriteRune(r)
		if r == '$' {
			q.pidx++
			placeholders++
			q.buf.WriteString(strconv.Itoa(q.pidx)) // SQL placeholders are 1-based
		}
	}

	if placeholders != len(params) {
		q.errs = append(q.errs,
			errors.Errorf("# of placeholders %d != # of params %d", placeholders, len(params)))
	}
	for i, param := range params {
		q.qargs.SetValue(fmt.Sprint(firstpidx+i+1), param)
	}
}

// resultScanner scans columns from sql.ResultRow instances into variables,
// performing the appropriate casting and error detection along the way.
type resultScanner struct {
	colNameToIdx map[string]int
}

func makeResultScanner(cols []sqlbase.ResultColumn) resultScanner {
	rs := resultScanner{
		colNameToIdx: make(map[string]int),
	}
	for i, col := range cols {
		rs.colNameToIdx[col.Name] = i
	}
	return rs
}

// IsNull returns whether the specified column of the given row contains
// a SQL NULL value.
func (rs resultScanner) IsNull(row parser.Datums, col string) (bool, error) {
	idx, ok := rs.colNameToIdx[col]
	if !ok {
		return false, errors.Errorf("result is missing column %s", col)
	}
	return row[idx] == parser.DNull, nil
}

// ScanIndex scans the given column index of the given row into dst.
func (rs resultScanner) ScanIndex(row parser.Datums, index int, dst interface{}) error {
	src := row[index]

	switch d := dst.(type) {
	case *string:
		if dst == nil {
			return errors.Errorf("nil destination pointer passed in")
		}
		s, ok := parser.AsDString(src)
		if !ok {
			return errors.Errorf("source type assertion failed")
		}
		*d = string(s)

	case *bool:
		if dst == nil {
			return errors.Errorf("nil destination pointer passed in")
		}
		s, ok := src.(*parser.DBool)
		if !ok {
			return errors.Errorf("source type assertion failed")
		}
		*d = bool(*s)

	case *int64:
		if dst == nil {
			return errors.Errorf("nil destination pointer passed in")
		}
		s, ok := parser.AsDInt(src)
		if !ok {
			return errors.Errorf("source type assertion failed")
		}
		*d = int64(s)

	case *time.Time:
		if dst == nil {
			return errors.Errorf("nil destination pointer passed in")
		}
		s, ok := src.(*parser.DTimestamp)
		if !ok {
			return errors.Errorf("source type assertion failed")
		}
		*d = s.Time

	case *[]byte:
		if dst == nil {
			return errors.Errorf("nil destination pointer passed in")
		}
		s, ok := src.(*parser.DBytes)
		if !ok {
			return errors.Errorf("source type assertion failed")
		}
		// Yes, this copies, but this probably isn't in the critical path.
		*d = []byte(*s)

	default:
		return errors.Errorf("unimplemented type for scanCol: %T", dst)
	}

	return nil
}

// Scan scans the column with the given name from the given row into dst.
func (rs resultScanner) Scan(row parser.Datums, colName string, dst interface{}) error {
	idx, ok := rs.colNameToIdx[colName]
	if !ok {
		return errors.Errorf("result is missing column %s", colName)
	}
	return rs.ScanIndex(row, idx, dst)
}

// TODO(mrtracy): The following methods, used to look up the zone configuration
// for a database or table, use the same algorithm as a set of methods in
// cli/zone.go for the same purpose. However, as that code connects to the
// server with a SQL connections, while this code uses a sql.Executor directly,
// the code cannot be commonized.
//
// Github issue #4869 is the most likely candidate for addressing this
// incompatibility; when that issue has been resolved, this code from
// cli/zone.go should be moved to a common location and shared with this system.

// queryZone retrieves the specific ZoneConfig associated with the supplied ID,
// if it exists.
func (s *adminServer) queryZone(
	ctx context.Context, session *sql.Session, id sqlbase.ID,
) (config.ZoneConfig, bool, error) {
	const query = `SELECT config FROM system.zones WHERE id = $1`
	params := parser.MakePlaceholderInfo()
	params.SetValue(`1`, parser.NewDInt(parser.DInt(id)))
	r := s.server.sqlExecutor.ExecuteStatements(session, query, &params)
	defer r.Close(ctx)
	if err := s.checkQueryResults(r.ResultList, 1); err != nil {
		return config.ZoneConfig{}, false, err
	}

	result := r.ResultList[0]
	if result.Rows.Len() == 0 {
		return config.ZoneConfig{}, false, nil
	}

	var zoneBytes []byte
	scanner := resultScanner{}
	err := scanner.ScanIndex(result.Rows.At(0), 0, &zoneBytes)
	if err != nil {
		return config.ZoneConfig{}, false, err
	}

	var zone config.ZoneConfig
	if err := zone.Unmarshal(zoneBytes); err != nil {
		return config.ZoneConfig{}, false, err
	}
	return zone, true, nil
}

// queryZonePath queries a path of sql object IDs, as generated by
// queryDescriptorIDPath(), for a ZoneConfig. It returns the most specific
// ZoneConfig specified for the object IDs in the path.
func (s *adminServer) queryZonePath(
	ctx context.Context, session *sql.Session, path []sqlbase.ID,
) (sqlbase.ID, config.ZoneConfig, bool, error) {
	for i := len(path) - 1; i >= 0; i-- {
		zone, zoneExists, err := s.queryZone(ctx, session, path[i])
		if err != nil || zoneExists {
			return path[i], zone, true, err
		}
	}
	return 0, config.ZoneConfig{}, false, nil
}

// queryNamespaceID queries for the ID of the namespace with the given name and
// parent ID.
func (s *adminServer) queryNamespaceID(
	ctx context.Context, session *sql.Session, parentID sqlbase.ID, name string,
) (sqlbase.ID, error) {
	const query = `SELECT id FROM system.namespace WHERE "parentID" = $1 AND name = $2`
	params := parser.MakePlaceholderInfo()
	params.SetValue(`1`, parser.NewDInt(parser.DInt(parentID)))
	params.SetValue(`2`, parser.NewDString(name))
	r := s.server.sqlExecutor.ExecuteStatements(session, query, &params)
	defer r.Close(ctx)
	if err := s.checkQueryResults(r.ResultList, 1); err != nil {
		return 0, err
	}

	result := r.ResultList[0]
	if result.Rows.Len() == 0 {
		return 0, errors.Errorf("namespace %s with ParentID %d not found", name, parentID)
	}

	var id int64
	scanner := resultScanner{}
	err := scanner.ScanIndex(result.Rows.At(0), 0, &id)
	if err != nil {
		return 0, err
	}

	return sqlbase.ID(id), nil
}

// queryDescriptorIDPath converts a path of namespaces into a path of namespace
// IDs. For example, if this function is called with a database/table name pair,
// it will return a list of IDs consisting of the root namespace ID, the
// databases ID, and the table ID (in that order).
func (s *adminServer) queryDescriptorIDPath(
	ctx context.Context, session *sql.Session, names []string,
) ([]sqlbase.ID, error) {
	path := []sqlbase.ID{keys.RootNamespaceID}
	for _, name := range names {
		id, err := s.queryNamespaceID(ctx, session, path[len(path)-1], name)
		if err != nil {
			return nil, err
		}
		path = append(path, id)
	}
	return path, nil
}

// assertNotVirtualSchema checks if the provided database name corresponds to a
// virtual schema, and if so, returns an error.
func (s *adminServer) assertNotVirtualSchema(dbName string) error {
	if s.server.sqlExecutor.IsVirtualDatabase(dbName) {
		return grpc.Errorf(codes.InvalidArgument, "%q is a virtual schema", dbName)
	}
	return nil
}
