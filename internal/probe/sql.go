package probe

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The sql probe sends exactly one read statement over the kernel's own
// connection, opened for this probe and closed after it. The statement runs
// inside a READ ONLY transaction with a statement timeout; that is
// auxiliary. The guard is the role the DSN logs in as (§3.3): SELECT on
// content-free views, EXECUTE revoked. The refusals below stop the shapes
// that a SELECT-only grant would otherwise let through unnoticed.
var (
	sqlForbiddenFunction = regexp.MustCompile(`(?i)\b(pg_advisory_[a-z_]*|pg_try_advisory_[a-z_]*|pg_notify|pg_terminate_backend|pg_cancel_backend|dblink[a-z_]*|lo_[a-z_]+|pg_read_[a-z_]+|pg_ls_[a-z_]+|pg_stat_file|set_config|pg_reload_conf|pg_switch_wal|pg_create_[a-z_]+|pg_drop_[a-z_]+|pg_logical_[a-z_]+|txid_[a-z_]+|pg_export_snapshot|pg_sleep[a-z_]*)\s*\(`)
	sqlForbiddenClause   = regexp.MustCompile(`(?i)\b(EXPLAIN|INTO|FOR\s+(?:NO\s+KEY\s+)?UPDATE|FOR\s+(?:KEY\s+)?SHARE|LOCK|COPY|SET|RESET|DO|CALL|WITH\s+RECURSIVE)\b`)
)

// sqlStatementProblem names why a statement is refused before it is sent.
// The catalogue already required a single SELECT without a separator.
func sqlStatementProblem(statement string) string {
	if m := sqlForbiddenFunction.FindStringSubmatch(statement); m != nil {
		return fmt.Sprintf("function %s is not allowed", strings.ToLower(m[1]))
	}
	if m := sqlForbiddenClause.FindStringSubmatch(statement); m != nil {
		return fmt.Sprintf("%s is not part of a read", strings.ToUpper(strings.Join(strings.Fields(m[1]), " ")))
	}
	if strings.Count(statement, "'")%2 != 0 {
		return "unbalanced quote"
	}
	return ""
}

// sqlConnector opens one connection per probe. The pgx implementation is
// the real one; tests substitute a fake that records what was sent.
type sqlConnector interface {
	connect(ctx context.Context, dsn string) (sqlConn, error)
}

type sqlConn interface {
	exec(ctx context.Context, statement string) error
	query(ctx context.Context, statement string) (columns []string, rows [][]string, err error)
	close(ctx context.Context) error
}

type pgxConnector struct{}

func (pgxConnector) connect(ctx context.Context, dsn string) (sqlConn, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// The extended protocol carries one statement per message; a string
	// with two statements is refused by the server ("cannot insert multiple
	// commands into a prepared statement"), which is the structural half of
	// "one statement only".
	config.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	return pgxConn{conn: conn}, nil
}

type pgxConn struct{ conn *pgx.Conn }

func (c pgxConn) exec(ctx context.Context, statement string) error {
	_, err := c.conn.Exec(ctx, statement)
	return err
}

func (c pgxConn) query(ctx context.Context, statement string) ([]string, [][]string, error) {
	rows, err := c.conn.Query(ctx, statement)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	descriptions := rows.FieldDescriptions()
	columns := make([]string, 0, len(descriptions))
	for _, description := range descriptions {
		columns = append(columns, description.Name)
	}
	var out [][]string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return columns, out, err
		}
		row := make([]string, 0, len(values))
		for _, value := range values {
			row = append(row, fmt.Sprint(value))
		}
		out = append(out, row)
		if len(out) >= sqlMaxRows {
			break
		}
	}
	return columns, out, rows.Err()
}

func (c pgxConn) close(ctx context.Context) error { return c.conn.Close(ctx) }

const sqlMaxRows = 1000

// runSQL opens the connection, fences the transaction, sends the one
// statement and renders the rows as tab-separated text.
func runSQL(ctx context.Context, plan Plan, dsn string, connector sqlConnector) execResult {
	statement := plan.Args["query"]
	if reason := sqlStatementProblem(statement); reason != "" {
		return execResult{exitCode: -1, failure: "refused: " + reason}
	}
	if dsn == "" {
		return execResult{exitCode: -1, failure: fmt.Sprintf("%s is not set for the kernel", plan.Spec.DSNEnv)}
	}
	timeout := time.Duration(plan.Spec.Timeout()) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := connector.connect(ctx, dsn)
	if err != nil {
		return execResult{exitCode: -1, failure: "connect: " + safeDBError(err)}
	}
	defer func() { _ = conn.close(context.Background()) }()
	statementTimeout := plan.Spec.StatementTimeoutMS
	if statementTimeout <= 0 {
		statementTimeout = DefaultSQLStatementTimeoutMS
	}
	for _, fence := range []string{
		"BEGIN READ ONLY",
		fmt.Sprintf("SET LOCAL statement_timeout = %d", statementTimeout),
		"SET LOCAL lock_timeout = 2000",
		"SET LOCAL transaction_read_only = on",
	} {
		if err := conn.exec(ctx, fence); err != nil {
			return execResult{exitCode: -1, failure: "fence: " + safeDBError(err)}
		}
	}
	columns, rows, err := conn.query(ctx, statement)
	_ = conn.exec(context.Background(), "ROLLBACK")
	writer := &cappedWriter{cap: plan.Spec.MaxOutput()}
	if len(columns) > 0 {
		_, _ = writer.Write([]byte(strings.Join(columns, "\t") + "\n"))
	}
	for _, row := range rows {
		_, _ = writer.Write([]byte(strings.Join(row, "\t") + "\n"))
	}
	result := execResult{output: writer.buf.String(), total: writer.total, truncated: writer.truncated()}
	if err != nil {
		result.exitCode = 1
		result.failure = safeDBError(err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.timedOut = true
		}
	}
	return result
}

// safeDBError keeps the server's message but never the DSN.
func safeDBError(err error) string {
	message := err.Error()
	if at := strings.Index(message, "://"); at >= 0 {
		return "database error (details withheld: message carried a connection string)"
	}
	return message
}
