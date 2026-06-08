package httpserver

import "database/sql"

// sessionsDB gives the dashboard a lightweight way to read
// schema_version without growing the public surface of every
// service. The session service embeds *sql.DB via *db.DB.
func (s *Server) sessionsDB() *sql.DB {
	// session.Service is constructed with the db handle and
	// passes through QueryRowContext via the embedded *sql.DB
	// inside db.DB. We do not have direct access to it from
	// this package, so we route through a small helper interface
	// exposed by the platformconfig service which holds the
	// same handle. To avoid a hard cross-dep we keep this
	// minimal: there is exactly one *sql.DB in the process.
	// platformconfig.Service wraps the DB; expose a thin
	// accessor via its public API instead.
	return s.platformCfg.DB()
}
