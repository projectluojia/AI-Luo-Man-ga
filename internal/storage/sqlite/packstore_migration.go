package sqlite

// 迁移 25：通用包文档存储（ailuo.store）替换 bus 专属关系表。数据权威元数据
// （来源修订/权威性/有效期）随 namespace 通用化；历史 bus 数据前向迁移到
// namespace 'campus/bus' 的文档集合，bus 专属表与触发器删除。

func init() {
	registerMigration(25, `
CREATE TABLE package_documents (
  app_id TEXT NOT NULL,
  namespace TEXT NOT NULL,
  collection TEXT NOT NULL,
  doc_id TEXT NOT NULL,
  payload TEXT NOT NULL CHECK(json_valid(payload) AND length(payload) <= 65536),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (app_id, namespace, collection, doc_id)
);
CREATE INDEX package_documents_scope_idx ON package_documents(app_id, namespace, collection, doc_id);

CREATE TABLE package_snapshots (
  app_id TEXT NOT NULL,
  namespace TEXT NOT NULL,
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  source TEXT NOT NULL CHECK(length(source) BETWEEN 1 AND 256),
  authoritative INTEGER NOT NULL CHECK(authoritative IN (0,1)),
  complete INTEGER NOT NULL CHECK(complete IN (0,1)),
  imported_at TEXT NOT NULL,
  valid_until TEXT NOT NULL,
  is_current INTEGER NOT NULL CHECK(is_current IN (0,1)),
  PRIMARY KEY (app_id, namespace, revision)
);
CREATE UNIQUE INDEX package_snapshots_current_idx
  ON package_snapshots(app_id, namespace) WHERE is_current = 1;

WITH RECURSIVE stop_aliases(app_id, stop_id, rest, alias, position) AS (
  SELECT app_id, id, aliases || char(31), NULL, 0
  FROM bus_stops
  WHERE aliases <> ''
  UNION ALL
  SELECT app_id, stop_id,
    substr(rest, instr(rest, char(31)) + 1),
    substr(rest, 1, instr(rest, char(31)) - 1),
    position + 1
  FROM stop_aliases
  WHERE rest <> ''
)
INSERT INTO package_documents(app_id, namespace, collection, doc_id, payload, updated_at)
SELECT stops.app_id, 'campus/bus', 'stops', stops.id,
  json_object('id', stops.id, 'name', stops.name, 'aliases',
    CASE WHEN stops.aliases = '' THEN json('[]')
      ELSE (SELECT json_group_array(alias)
        FROM (SELECT alias
          FROM stop_aliases
          WHERE stop_aliases.app_id = stops.app_id AND stop_aliases.stop_id = stops.id
            AND stop_aliases.alias IS NOT NULL
          ORDER BY position))
    END,
    'latitude', stops.latitude, 'longitude', stops.longitude,
    'source_revision', stops.source_revision),
  strftime('%Y-%m-%dT%H:%M:%fZ','now')
FROM bus_stops stops;

INSERT INTO package_documents(app_id, namespace, collection, doc_id, payload, updated_at)
SELECT app_id, 'campus/bus', 'routes', id,
  json_object('id', id, 'name', name, 'direction', direction,
    'origin_stop_id', origin_stop_id, 'destination_stop_id', destination_stop_id,
    'source_revision', source_revision),
  strftime('%Y-%m-%dT%H:%M:%fZ','now')
FROM bus_routes;

INSERT INTO package_documents(app_id, namespace, collection, doc_id, payload, updated_at)
SELECT app_id, 'campus/bus', 'journeys', trip_id || '-' || origin_stop_id || '-' || destination_stop_id,
  json_object('trip_id', trip_id, 'route_id', route_id, 'route_name', route_name,
    'direction', direction, 'origin_stop_id', origin_stop_id, 'origin_stop_name', origin_stop_name,
    'destination_stop_id', destination_stop_id, 'destination_stop_name', destination_stop_name,
    'departure_at', departure_at, 'arrival_at', arrival_at, 'source_revision', source_revision),
  strftime('%Y-%m-%dT%H:%M:%fZ','now')
FROM bus_journeys;

INSERT INTO package_snapshots(app_id, namespace, revision, source, authoritative, complete, imported_at, valid_until, is_current)
SELECT current.app_id, 'campus/bus', source.revision, source.source, source.authoritative, source.complete,
  source.imported_at, coalesce(source.valid_until, source.imported_at), 1
FROM bus_current_snapshots current
JOIN bus_source_revisions source
  ON source.app_id = current.app_id AND source.revision = current.revision;

DROP TABLE bus_journeys;
DROP TABLE bus_routes;
DROP TABLE bus_stops;
DROP TABLE bus_current_snapshots;
DROP TABLE bus_source_revisions;
`)
}
