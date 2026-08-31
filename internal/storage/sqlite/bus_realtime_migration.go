package sqlite

func init() {
	registerMigration(27, `
ALTER TABLE bus_source_revisions ADD COLUMN realtime_authorized INTEGER NOT NULL DEFAULT 0 CHECK(realtime_authorized IN (0,1));
CREATE TABLE bus_vehicle_positions (
  app_id TEXT NOT NULL,
  vehicle_id TEXT NOT NULL,
  route_id TEXT NOT NULL,
  latitude REAL NOT NULL CHECK(latitude BETWEEN -90 AND 90),
  longitude REAL NOT NULL CHECK(longitude BETWEEN -180 AND 180),
  recorded_at TEXT NOT NULL,
  source_revision TEXT NOT NULL,
  PRIMARY KEY(app_id, vehicle_id, source_revision),
  FOREIGN KEY(app_id, source_revision) REFERENCES bus_source_revisions(app_id, revision) ON DELETE CASCADE
);
CREATE INDEX bus_vehicle_positions_route_idx ON bus_vehicle_positions(app_id, source_revision, route_id, recorded_at);
`)
}
