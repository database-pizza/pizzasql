package executor

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// PostgreSQL catalog emulation.
//
// pg_catalog and information_schema are synthesized from the durable schema the
// connection's session observes (including DDL staged by an open transaction).
// Nothing is persisted and no virtual relation accepts writes. OIDs are
// deterministic: a 32-bit FNV-1a hash of the namespace-qualified name, lifted
// above PostgreSQL's FirstNormalObjectId and collision-probed in sorted order.
// The same allocator feeds pg_class, pg_attribute, pg_type, pg_index, and
// pg_constraint, so catalog joins agree.
//
// The emulated schemas are pg_catalog (OID 11), public (OID 2200), and
// information_schema (OID 13207). User relations always live in public.

const (
	pgCatalogSchemaName    = "pg_catalog"
	informationSchemaName  = "information_schema"
	publicSchemaName       = "public"
	pgCatalogNamespaceOID  = int64(11)
	publicNamespaceOID     = int64(2200)
	infoSchemaNamespaceOID = int64(13207)

	firstNormalObjectID = int64(16384)

	// Built-in OIDs the emulation refers to.
	pgDefaultTablespaceOID = int64(1663)
	pgGlobalTablespaceOID  = int64(1664)
	pgBtreeAMOID           = int64(403)
	pgHeapAMOID            = int64(2)
	catalogOwnerOID        = int64(10)
)

// catalogRelationDef declares a virtual relation's column list.
type catalogRelationDef struct {
	schema  string
	name    string
	columns []catalogColumnDef
}

type catalogColumnDef struct {
	name string
	typ  string
}

// pgCatalogRelationDefs lists the read-only pg_catalog relations.
var pgCatalogRelationDefs = []catalogRelationDef{
	{schema: pgCatalogSchemaName, name: "pg_namespace", columns: []catalogColumnDef{
		{"oid", "OID"}, {"nspname", "NAME"}, {"nspowner", "OID"}, {"nspacl", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_class", columns: []catalogColumnDef{
		{"oid", "OID"}, {"relname", "NAME"}, {"relnamespace", "OID"}, {"reltype", "OID"},
		{"reloftype", "OID"}, {"relowner", "OID"}, {"relam", "OID"}, {"relfilenode", "OID"},
		{"reltablespace", "OID"}, {"relpages", "INT4"}, {"reltuples", "FLOAT4"},
		{"relallvisible", "INT4"}, {"reltoastrelid", "OID"}, {"relhasindex", "BOOL"},
		{"relisshared", "BOOL"}, {"relpersistence", "CHAR"}, {"relkind", "CHAR"},
		{"relnatts", "INT2"}, {"relchecks", "INT2"}, {"relhasrules", "BOOL"},
		{"relhastriggers", "BOOL"}, {"relhassubclass", "BOOL"}, {"relrowsecurity", "BOOL"},
		{"relforcerowsecurity", "BOOL"}, {"relispopulated", "BOOL"}, {"relreplident", "CHAR"},
		{"relispartition", "BOOL"}, {"relrewrite", "OID"}, {"relfrozenxid", "OID"},
		{"relminmxid", "OID"}, {"relacl", "TEXT"}, {"reloptions", "TEXT"},
		{"relpartbound", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_attribute", columns: []catalogColumnDef{
		{"attrelid", "OID"}, {"attname", "NAME"}, {"atttypid", "OID"},
		{"attstattarget", "INT4"}, {"attlen", "INT2"}, {"attnum", "INT2"},
		{"attndims", "INT4"}, {"attcacheoff", "INT4"}, {"atttypmod", "INT4"},
		{"attbyval", "BOOL"}, {"attalign", "CHAR"}, {"attstorage", "CHAR"},
		{"attcompression", "CHAR"}, {"attnotnull", "BOOL"}, {"atthasdef", "BOOL"},
		{"atthasmissing", "BOOL"}, {"attidentity", "CHAR"}, {"attgenerated", "CHAR"},
		{"attisdropped", "BOOL"}, {"attislocal", "BOOL"}, {"attinhcount", "INT2"},
		{"attcollation", "OID"}, {"attacl", "TEXT"}, {"attoptions", "TEXT"},
		{"attfdwoptions", "TEXT"}, {"attmissingval", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_type", columns: []catalogColumnDef{
		{"oid", "OID"}, {"typname", "NAME"}, {"typnamespace", "OID"},
		{"typowner", "OID"}, {"typlen", "INT2"}, {"typbyval", "BOOL"},
		{"typtype", "CHAR"}, {"typcategory", "CHAR"}, {"typispreferred", "BOOL"},
		{"typisdefined", "BOOL"}, {"typdelim", "CHAR"}, {"typrelid", "OID"},
		{"typsubscript", "REGPROC"}, {"typelem", "OID"}, {"typarray", "OID"},
		{"typinput", "REGPROC"}, {"typoutput", "REGPROC"}, {"typreceive", "REGPROC"},
		{"typsend", "REGPROC"}, {"typmodin", "REGPROC"}, {"typmodout", "REGPROC"},
		{"typanalyze", "REGPROC"}, {"typalign", "CHAR"}, {"typstorage", "CHAR"},
		{"typnotnull", "BOOL"}, {"typbasetype", "OID"}, {"typtypmod", "INT4"},
		{"typndims", "INT4"}, {"typcollation", "OID"}, {"typdefaultbin", "TEXT"},
		{"typdefault", "TEXT"}, {"typacl", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_index", columns: []catalogColumnDef{
		{"indexrelid", "OID"}, {"indrelid", "OID"}, {"indnatts", "INT2"},
		{"indnkeyatts", "INT2"}, {"indisunique", "BOOL"}, {"indisprimary", "BOOL"},
		{"indisexclusion", "BOOL"}, {"indimmediate", "BOOL"}, {"indisclustered", "BOOL"},
		{"indisvalid", "BOOL"}, {"indcheckxmin", "BOOL"}, {"indisready", "BOOL"},
		{"indislive", "BOOL"}, {"indisreplident", "BOOL"}, {"indkey", "INT2[]"},
		{"indcollation", "TEXT"}, {"indclass", "TEXT"}, {"indoption", "INT2[]"},
		{"indexprs", "TEXT"}, {"indpred", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_constraint", columns: []catalogColumnDef{
		{"oid", "OID"}, {"conname", "NAME"}, {"connamespace", "OID"},
		{"contype", "CHAR"}, {"condeferrable", "BOOL"}, {"condeferred", "BOOL"},
		{"convalidated", "BOOL"}, {"conrelid", "OID"}, {"contypid", "OID"},
		{"conindid", "OID"}, {"conparentid", "OID"}, {"confrelid", "OID"},
		{"confupdtype", "CHAR"}, {"confdeltype", "CHAR"}, {"confmatchtype", "CHAR"},
		{"conislocal", "BOOL"}, {"coninhcount", "INT2"}, {"connoinherit", "BOOL"},
		{"conkey", "INT2[]"}, {"confkey", "INT2[]"}, {"conpfeqop", "TEXT"},
		{"conppeqop", "TEXT"}, {"conffeqop", "TEXT"}, {"conexclop", "TEXT"},
		{"conbin", "TEXT"}, {"consrc", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_attrdef", columns: []catalogColumnDef{
		{"oid", "OID"}, {"adrelid", "OID"}, {"adnum", "INT2"}, {"adbin", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_database", columns: []catalogColumnDef{
		{"oid", "OID"}, {"datname", "NAME"}, {"datdba", "OID"}, {"encoding", "INT4"},
		{"datlocprovider", "CHAR"}, {"datistemplate", "BOOL"}, {"datallowconn", "BOOL"},
		{"datconnlimit", "INT4"}, {"datlastsysoid", "OID"}, {"datfrozenxid", "OID"},
		{"datminmxid", "OID"}, {"dattablespace", "OID"}, {"datcollate", "TEXT"},
		{"datctype", "TEXT"}, {"datlocale", "TEXT"}, {"daticurules", "TEXT"},
		{"datcollversion", "TEXT"}, {"datacl", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_tablespace", columns: []catalogColumnDef{
		{"oid", "OID"}, {"spcname", "NAME"}, {"spcowner", "OID"}, {"spcacl", "TEXT"},
		{"spcoptions", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_proc", columns: []catalogColumnDef{
		{"oid", "OID"}, {"proname", "NAME"}, {"pronamespace", "OID"},
		{"proowner", "OID"}, {"prolang", "OID"}, {"procost", "FLOAT4"},
		{"prorows", "FLOAT4"}, {"provariadic", "OID"}, {"prosupport", "REGPROC"},
		{"prokind", "CHAR"}, {"prosecdef", "BOOL"}, {"proleakproof", "BOOL"},
		{"proisstrict", "BOOL"}, {"proretset", "BOOL"}, {"provolatile", "CHAR"},
		{"proparallel", "CHAR"}, {"pronargs", "INT2"}, {"pronargdefaults", "INT2"},
		{"prorettype", "OID"}, {"proargtypes", "TEXT"}, {"proallargtypes", "TEXT"},
		{"proargmodes", "TEXT"}, {"proargnames", "TEXT"}, {"proargdefaults", "TEXT"},
		{"protrftypes", "TEXT"}, {"prosrc", "TEXT"}, {"probin", "TEXT"},
		{"prosqlbody", "TEXT"}, {"proconfig", "TEXT"}, {"proacl", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_am", columns: []catalogColumnDef{
		{"oid", "OID"}, {"amname", "NAME"}, {"amhandler", "OID"}, {"amtype", "CHAR"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_description", columns: []catalogColumnDef{
		{"objoid", "OID"}, {"classoid", "OID"}, {"objsubid", "INT4"}, {"description", "TEXT"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_tables", columns: []catalogColumnDef{
		{"schemaname", "NAME"}, {"tablename", "NAME"}, {"tableowner", "NAME"},
		{"tablespace", "NAME"}, {"hasindexes", "BOOL"}, {"hasrules", "BOOL"},
		{"hastriggers", "BOOL"}, {"rowsecurity", "BOOL"},
	}},
	{schema: pgCatalogSchemaName, name: "pg_indexes", columns: []catalogColumnDef{
		{"schemaname", "NAME"}, {"tablename", "NAME"}, {"indexname", "NAME"},
		{"tablespace", "NAME"}, {"indexdef", "TEXT"},
	}},
}

// informationSchemaRelationDefs lists the read-only information_schema views.
var informationSchemaRelationDefs = []catalogRelationDef{
	{schema: informationSchemaName, name: "tables", columns: []catalogColumnDef{
		{"table_catalog", "TEXT"}, {"table_schema", "TEXT"}, {"table_name", "TEXT"},
		{"table_type", "TEXT"}, {"self_referencing_column_name", "TEXT"},
		{"reference_generation", "TEXT"}, {"user_defined_type_catalog", "TEXT"},
		{"user_defined_type_schema", "TEXT"}, {"user_defined_type_name", "TEXT"},
		{"is_insertable_into", "TEXT"}, {"is_typed", "TEXT"}, {"commit_action", "TEXT"},
	}},
	{schema: informationSchemaName, name: "columns", columns: []catalogColumnDef{
		{"table_catalog", "TEXT"}, {"table_schema", "TEXT"}, {"table_name", "TEXT"},
		{"column_name", "TEXT"}, {"ordinal_position", "INT4"}, {"column_default", "TEXT"},
		{"is_nullable", "TEXT"}, {"data_type", "TEXT"}, {"character_maximum_length", "INT4"},
		{"character_octet_length", "INT4"}, {"numeric_precision", "INT4"},
		{"numeric_precision_radix", "INT4"}, {"numeric_scale", "INT4"},
		{"datetime_precision", "INT4"}, {"interval_type", "TEXT"},
		{"interval_precision", "INT4"}, {"character_set_catalog", "TEXT"},
		{"character_set_schema", "TEXT"}, {"character_set_name", "TEXT"},
		{"collation_catalog", "TEXT"}, {"collation_schema", "TEXT"},
		{"collation_name", "TEXT"}, {"domain_catalog", "TEXT"}, {"domain_schema", "TEXT"},
		{"domain_name", "TEXT"}, {"udt_catalog", "TEXT"}, {"udt_schema", "TEXT"},
		{"udt_name", "TEXT"}, {"scope_catalog", "TEXT"}, {"scope_schema", "TEXT"},
		{"scope_name", "TEXT"}, {"maximum_cardinality", "INT4"}, {"dtd_identifier", "TEXT"},
		{"is_self_referencing", "TEXT"}, {"is_identity", "TEXT"},
		{"identity_generation", "TEXT"}, {"identity_start", "TEXT"},
		{"identity_increment", "TEXT"}, {"identity_maximum", "TEXT"},
		{"identity_minimum", "TEXT"}, {"identity_cycle", "TEXT"},
		{"is_generated", "TEXT"}, {"generation_expression", "TEXT"},
		{"is_updatable", "TEXT"},
	}},
	{schema: informationSchemaName, name: "table_constraints", columns: []catalogColumnDef{
		{"constraint_catalog", "TEXT"}, {"constraint_schema", "TEXT"},
		{"constraint_name", "TEXT"}, {"table_catalog", "TEXT"}, {"table_schema", "TEXT"},
		{"table_name", "TEXT"}, {"constraint_type", "TEXT"}, {"is_deferrable", "TEXT"},
		{"initially_deferred", "TEXT"}, {"enforced", "TEXT"}, {"nulls_distinct", "TEXT"},
	}},
	{schema: informationSchemaName, name: "key_column_usage", columns: []catalogColumnDef{
		{"constraint_catalog", "TEXT"}, {"constraint_schema", "TEXT"},
		{"constraint_name", "TEXT"}, {"table_catalog", "TEXT"}, {"table_schema", "TEXT"},
		{"table_name", "TEXT"}, {"column_name", "TEXT"}, {"ordinal_position", "INT4"},
		{"position_in_unique_constraint", "INT4"},
	}},
	{schema: informationSchemaName, name: "referential_constraints", columns: []catalogColumnDef{
		{"constraint_catalog", "TEXT"}, {"constraint_schema", "TEXT"},
		{"constraint_name", "TEXT"}, {"unique_constraint_catalog", "TEXT"},
		{"unique_constraint_schema", "TEXT"}, {"unique_constraint_name", "TEXT"},
		{"match_option", "TEXT"}, {"update_rule", "TEXT"}, {"delete_rule", "TEXT"},
		{"is_deferrable", "TEXT"}, {"initially_deferred", "TEXT"}, {"enforced", "TEXT"},
	}},
	{schema: informationSchemaName, name: "constraint_column_usage", columns: []catalogColumnDef{
		{"table_catalog", "TEXT"}, {"table_schema", "TEXT"}, {"table_name", "TEXT"},
		{"constraint_catalog", "TEXT"}, {"constraint_schema", "TEXT"},
		{"constraint_name", "TEXT"}, {"column_name", "TEXT"},
	}},
	{schema: informationSchemaName, name: "schemata", columns: []catalogColumnDef{
		{"catalog_name", "TEXT"}, {"schema_name", "TEXT"}, {"schema_owner", "TEXT"},
		{"default_character_set_catalog", "TEXT"},
		{"default_character_set_schema", "TEXT"},
		{"default_character_set_name", "TEXT"}, {"sql_path", "TEXT"},
	}},
	{schema: informationSchemaName, name: "sequences", columns: []catalogColumnDef{
		{"sequence_catalog", "TEXT"}, {"sequence_schema", "TEXT"},
		{"sequence_name", "TEXT"}, {"data_type", "TEXT"},
		{"numeric_precision", "INT4"}, {"numeric_precision_radix", "INT4"},
		{"numeric_scale", "INT4"}, {"start_value", "TEXT"}, {"minimum_value", "TEXT"},
		{"maximum_value", "TEXT"}, {"increment", "TEXT"}, {"cycle_option", "TEXT"},
	}},
	{schema: informationSchemaName, name: "views", columns: []catalogColumnDef{
		{"table_catalog", "TEXT"}, {"table_schema", "TEXT"}, {"table_name", "TEXT"},
		{"view_definition", "TEXT"}, {"check_option", "TEXT"}, {"is_updatable", "TEXT"},
		{"is_insertable_into", "TEXT"}, {"is_trigger_updatable", "TEXT"},
		{"is_trigger_deletable", "TEXT"}, {"is_trigger_insertable_into", "TEXT"},
	}},
}

// catalogRelationSchema builds the storage schema that the analyzer and the
// materialized SELECT path use for a virtual relation.
func catalogRelationSchema(def catalogRelationDef) *storage.Schema {
	schema := &storage.Schema{Name: def.name, Columns: make([]storage.Column, len(def.columns))}
	for i, column := range def.columns {
		schema.Columns[i] = storage.Column{Name: column.name, Type: column.typ, Nullable: true}
	}
	return schema
}

var pgCatalogRelationIndex = buildCatalogRelationIndex()

type catalogRelationEntry struct {
	def    catalogRelationDef
	schema *storage.Schema
}

func buildCatalogRelationIndex() map[string]catalogRelationEntry {
	index := make(map[string]catalogRelationEntry, len(pgCatalogRelationDefs)+len(informationSchemaRelationDefs))
	for _, def := range append(append([]catalogRelationDef(nil), pgCatalogRelationDefs...), informationSchemaRelationDefs...) {
		index[def.schema+"."+strings.ToLower(def.name)] = catalogRelationEntry{def: def, schema: catalogRelationSchema(def)}
	}
	return index
}

// catalogRelationFor resolves a possibly schema-qualified table reference to a
// virtual catalog relation. A CTE in scope shadows every relation name, and an
// unqualified name resolves to pg_catalog only when no durable table shadows it,
// matching PostgreSQL's implicit search_path.
func (e *Executor) catalogRelationFor(ref parser.TableRef) (catalogRelationEntry, bool) {
	if ref.Subquery != nil || ref.Function != nil {
		return catalogRelationEntry{}, false
	}
	if e.cteTableExists(ref.Name) {
		return catalogRelationEntry{}, false
	}
	name := strings.ToLower(ref.Name)
	switch strings.ToLower(ref.Schema) {
	case pgCatalogSchemaName:
		entry, ok := pgCatalogRelationIndex[pgCatalogSchemaName+"."+name]
		return entry, ok
	case informationSchemaName:
		entry, ok := pgCatalogRelationIndex[informationSchemaName+"."+name]
		return entry, ok
	case "":
		if e.session.TableExists(ref.Name) {
			return catalogRelationEntry{}, false
		}
		entry, ok := pgCatalogRelationIndex[pgCatalogSchemaName+"."+name]
		return entry, ok
	default:
		// The emulated schemas are distinct: public.<name> names a durable
		// relation, never pg_catalog.<name>, so a qualified miss falls through to
		// the durable-schema path and is reported as an undefined table.
		return catalogRelationEntry{}, false
	}
}

// isCatalogRelationRef reports whether a FROM entry names a virtual relation.
func (e *Executor) isCatalogRelationRef(ref parser.TableRef) bool {
	_, ok := e.catalogRelationFor(ref)
	return ok
}

// fromReferencesCatalogRelation walks a FROM clause, including JOIN chains.
func (e *Executor) fromReferencesCatalogRelation(from []parser.TableRef) bool {
	for _, ref := range from {
		cur := ref
		for {
			if e.isCatalogRelationRef(cur) {
				return true
			}
			if cur.Join == nil || cur.Join.Table == nil {
				break
			}
			cur = *cur.Join.Table
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Deterministic OIDs

func fnv1a32(text string) uint32 {
	hash := uint32(2166136261)
	for i := 0; i < len(text); i++ {
		hash ^= uint32(text[i])
		hash *= 16777619
	}
	return hash
}

// catalogRelationKey identifies a relation row. relkind is part of the identity:
// the engine lets an index share its table's name, and both rows must keep their
// own OID so pg_class, pg_attribute, and pg_index agree, exactly as relname plus
// relkind identifies a pg_class row in PostgreSQL.
func catalogRelationKey(relation *catalogRelation) string {
	return strings.ToLower(relation.schemaName+"."+relation.name) + "#" + string(relation.kind)
}

// allocateOID maps a kind and fully qualified name to a stable, positive OID.
// The first probe is the FNV-1a hash lifted into [firstNormalObjectID,
// MaxInt32]; collisions are resolved by linear probing in the caller's sorted
// order, so the assignment is deterministic for a given schema.
func (s *catalogSnapshot) allocateOID(kind, key string) int64 {
	owner := kind + ":" + key
	oid := int64(fnv1a32("pizzasql:" + owner))
	oid &= 0x7fffffff
	if oid < firstNormalObjectID {
		oid += firstNormalObjectID
	}
	for {
		if existing, used := s.oidOwners[oid]; !used || existing == owner {
			s.oidOwners[oid] = owner
			return oid
		}
		oid++
		if oid > 0x7fffffff {
			oid = firstNormalObjectID
		}
	}
}

// ---------------------------------------------------------------------------
// Built-in type table

type pgBuiltinType struct {
	oid      int64
	name     string // pg_type.typname
	sqlName  string // format_type / information_schema spelling
	length   int64
	byval    bool
	category string
	delim    string
	elem     int64
	array    int64
	align    string
	storage  string
}

func (t pgBuiltinType) isArray() bool { return t.category == "A" }

var pgBuiltinTypes = []pgBuiltinType{
	{oid: 16, name: "bool", sqlName: "boolean", length: 1, byval: true, category: "B", delim: ",", array: 1000, align: "c", storage: "p"},
	{oid: 17, name: "bytea", sqlName: "bytea", length: -1, category: "U", delim: ",", array: 1001, align: "i", storage: "x"},
	{oid: 18, name: "char", sqlName: "\"char\"", length: 1, byval: true, category: "S", delim: ",", array: 1002, align: "c", storage: "p"},
	{oid: 19, name: "name", sqlName: "name", length: 64, category: "S", delim: ",", array: 1003, align: "c", storage: "p"},
	{oid: 20, name: "int8", sqlName: "bigint", length: 8, byval: true, category: "N", delim: ",", array: 1016, align: "d", storage: "p"},
	{oid: 21, name: "int2", sqlName: "smallint", length: 2, byval: true, category: "N", delim: ",", array: 1005, align: "s", storage: "p"},
	{oid: 23, name: "int4", sqlName: "integer", length: 4, byval: true, category: "N", delim: ",", array: 1007, align: "i", storage: "p"},
	{oid: 25, name: "text", sqlName: "text", length: -1, category: "S", delim: ",", array: 1009, align: "i", storage: "x"},
	{oid: 26, name: "oid", sqlName: "oid", length: 4, byval: true, category: "N", delim: ",", array: 1028, align: "i", storage: "p"},
	{oid: 114, name: "json", sqlName: "json", length: -1, category: "U", delim: ",", array: 199, align: "i", storage: "x"},
	{oid: 700, name: "float4", sqlName: "real", length: 4, byval: true, category: "N", delim: ",", array: 1021, align: "i", storage: "p"},
	{oid: 701, name: "float8", sqlName: "double precision", length: 8, byval: true, category: "N", delim: ",", array: 1022, align: "d", storage: "p"},
	{oid: 1042, name: "bpchar", sqlName: "character", length: -1, category: "S", delim: ",", array: 1014, align: "i", storage: "x"},
	{oid: 1043, name: "varchar", sqlName: "character varying", length: -1, category: "S", delim: ",", array: 1015, align: "i", storage: "x"},
	{oid: 1082, name: "date", sqlName: "date", length: 4, byval: true, category: "D", delim: ",", array: 1182, align: "i", storage: "p"},
	{oid: 1083, name: "time", sqlName: "time without time zone", length: 8, byval: true, category: "D", delim: ",", array: 1183, align: "d", storage: "p"},
	{oid: 1184, name: "timestamptz", sqlName: "timestamp with time zone", length: 8, byval: true, category: "D", delim: ",", array: 1185, align: "d", storage: "p"},
	{oid: 1700, name: "numeric", sqlName: "numeric", length: -1, category: "N", delim: ",", array: 1231, align: "i", storage: "x"},
	{oid: 2205, name: "regclass", sqlName: "regclass", length: 4, byval: true, category: "N", delim: ",", array: 2210, align: "i", storage: "p"},
	{oid: 2206, name: "regtype", sqlName: "regtype", length: 4, byval: true, category: "N", delim: ",", array: 2211, align: "i", storage: "p"},
	{oid: 2950, name: "uuid", sqlName: "uuid", length: 16, category: "U", delim: ",", array: 2951, align: "c", storage: "p"},
	{oid: 3802, name: "jsonb", sqlName: "jsonb", length: -1, category: "U", delim: ",", array: 3807, align: "i", storage: "x"},

	// Array types for the supported element types.
	{oid: 199, name: "_json", sqlName: "json[]", length: -1, category: "A", delim: ",", elem: 114, align: "i", storage: "x"},
	{oid: 1000, name: "_bool", sqlName: "boolean[]", length: -1, category: "A", delim: ",", elem: 16, align: "i", storage: "x"},
	{oid: 1001, name: "_bytea", sqlName: "bytea[]", length: -1, category: "A", delim: ",", elem: 17, align: "i", storage: "x"},
	{oid: 1002, name: "_char", sqlName: "\"char\"[]", length: -1, category: "A", delim: ",", elem: 18, align: "i", storage: "x"},
	{oid: 1003, name: "_name", sqlName: "name[]", length: -1, category: "A", delim: ",", elem: 19, align: "i", storage: "x"},
	{oid: 1005, name: "_int2", sqlName: "smallint[]", length: -1, category: "A", delim: ",", elem: 21, align: "i", storage: "x"},
	{oid: 1007, name: "_int4", sqlName: "integer[]", length: -1, category: "A", delim: ",", elem: 23, align: "i", storage: "x"},
	{oid: 1009, name: "_text", sqlName: "text[]", length: -1, category: "A", delim: ",", elem: 25, align: "i", storage: "x"},
	{oid: 1014, name: "_bpchar", sqlName: "character[]", length: -1, category: "A", delim: ",", elem: 1042, align: "i", storage: "x"},
	{oid: 1015, name: "_varchar", sqlName: "character varying[]", length: -1, category: "A", delim: ",", elem: 1043, align: "i", storage: "x"},
	{oid: 1016, name: "_int8", sqlName: "bigint[]", length: -1, category: "A", delim: ",", elem: 20, align: "i", storage: "x"},
	{oid: 1021, name: "_float4", sqlName: "real[]", length: -1, category: "A", delim: ",", elem: 700, align: "i", storage: "x"},
	{oid: 1022, name: "_float8", sqlName: "double precision[]", length: -1, category: "A", delim: ",", elem: 701, align: "i", storage: "x"},
	{oid: 1028, name: "_oid", sqlName: "oid[]", length: -1, category: "A", delim: ",", elem: 26, align: "i", storage: "x"},
	{oid: 1182, name: "_date", sqlName: "date[]", length: -1, category: "A", delim: ",", elem: 1082, align: "i", storage: "x"},
	{oid: 1183, name: "_time", sqlName: "time without time zone[]", length: -1, category: "A", delim: ",", elem: 1083, align: "i", storage: "x"},
	{oid: 1185, name: "_timestamptz", sqlName: "timestamp with time zone[]", length: -1, category: "A", delim: ",", elem: 1184, align: "i", storage: "x"},
	{oid: 1231, name: "_numeric", sqlName: "numeric[]", length: -1, category: "A", delim: ",", elem: 1700, align: "i", storage: "x"},
	{oid: 2210, name: "_regclass", sqlName: "regclass[]", length: -1, category: "A", delim: ",", elem: 2205, align: "i", storage: "x"},
	{oid: 2211, name: "_regtype", sqlName: "regtype[]", length: -1, category: "A", delim: ",", elem: 2206, align: "i", storage: "x"},
	{oid: 2951, name: "_uuid", sqlName: "uuid[]", length: -1, category: "A", delim: ",", elem: 2950, align: "i", storage: "x"},
	{oid: 3807, name: "_jsonb", sqlName: "jsonb[]", length: -1, category: "A", delim: ",", elem: 3802, align: "i", storage: "x"},
}

var pgBuiltinTypeByOID = func() map[int64]pgBuiltinType {
	index := make(map[int64]pgBuiltinType, len(pgBuiltinTypes))
	for _, t := range pgBuiltinTypes {
		index[t.oid] = t
	}
	return index
}()

var pgBuiltinTypeByName = func() map[string]pgBuiltinType {
	index := make(map[string]pgBuiltinType, len(pgBuiltinTypes))
	for _, t := range pgBuiltinTypes {
		index[t.name] = t
	}
	return index
}()

// pgTypeForDeclaredName maps an engine column type ("INTEGER", "TEXT[]", ...)
// to its PostgreSQL type row. Unknown names report TEXT rather than claiming a
// type the engine does not have.
func pgTypeForDeclaredName(name string) pgBuiltinType {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if idx := strings.IndexByte(upper, '('); idx >= 0 {
		upper = strings.TrimSpace(upper[:idx])
	}
	if strings.HasSuffix(upper, "[]") {
		element := strings.TrimSpace(strings.TrimSuffix(upper, "[]"))
		base := pgTypeForDeclaredName(element)
		if array, ok := pgBuiltinTypeByOID[base.array]; ok && base.array != 0 {
			return array
		}
		if fallback, ok := pgBuiltinTypeByOID[1009]; ok {
			return fallback
		}
	}
	switch upper {
	case "INTEGER", "INT", "INT4":
		return pgBuiltinTypeByOID[23]
	case "BIGINT", "INT8":
		return pgBuiltinTypeByOID[20]
	case "SMALLINT", "INT2", "TINYINT", "MEDIUMINT":
		return pgBuiltinTypeByOID[21]
	case "TEXT", "CLOB", "NCHAR", "NVARCHAR":
		return pgBuiltinTypeByOID[25]
	case "VARCHAR", "CHARACTER VARYING":
		return pgBuiltinTypeByOID[1043]
	case "CHAR", "CHARACTER":
		return pgBuiltinTypeByOID[1042]
	case "BOOLEAN", "BOOL":
		return pgBuiltinTypeByOID[16]
	case "REAL", "FLOAT", "FLOAT4":
		return pgBuiltinTypeByOID[700]
	case "DOUBLE", "DOUBLE PRECISION", "FLOAT8":
		return pgBuiltinTypeByOID[701]
	case "NUMERIC", "DECIMAL":
		return pgBuiltinTypeByOID[1700]
	case "JSONB":
		return pgBuiltinTypeByOID[3802]
	case "JSON":
		return pgBuiltinTypeByOID[114]
	case "BLOB", "BYTEA":
		return pgBuiltinTypeByOID[17]
	case "DATETIME", "TIMESTAMP", "TIMESTAMPTZ", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITHOUT TIME ZONE":
		return pgBuiltinTypeByOID[1184]
	case "DATE":
		return pgBuiltinTypeByOID[1082]
	case "TIME", "TIME WITHOUT TIME ZONE", "TIME WITH TIME ZONE":
		return pgBuiltinTypeByOID[1083]
	case "UUID":
		return pgBuiltinTypeByOID[2950]
	case "OID":
		return pgBuiltinTypeByOID[26]
	case "NAME":
		return pgBuiltinTypeByOID[19]
	case "REGCLASS":
		return pgBuiltinTypeByOID[2205]
	case "REGTYPE":
		return pgBuiltinTypeByOID[2206]
	default:
		return pgBuiltinTypeByOID[25]
	}
}

// ---------------------------------------------------------------------------
// Snapshot

type catalogRelation struct {
	schemaName string
	name       string
	oid        int64
	kind       byte // 'r' table, 'i' index, 'v' view / virtual relation
	columns    []storage.Column
	table      *storage.Schema
	index      *storage.Index
	// virtual marks a synthesized pg_catalog/information_schema relation. User
	// tables, indexes, and session views are the durable/session schema.
	virtual bool
	// primaryIndex marks the synthetic index that backs a PRIMARY KEY. The
	// engine does not persist an index object for the key, but PostgreSQL tools
	// expect pg_index/pg_constraint to point at one.
	primaryIndex bool
}

type catalogConstraint struct {
	oid      int64
	name     string
	table    *catalogRelation
	contype  byte
	indexOID int64
	columns  []string // child columns (PK/UNIQUE/FK) or empty for CHECK
	fk       *storage.ForeignKeyConstraint
	check    *storage.CheckConstraint
}

type catalogSnapshot struct {
	database string

	relations     []*catalogRelation
	relationByKey map[string]*catalogRelation
	relationByOID map[int64]*catalogRelation
	tableByName   map[string]*catalogRelation
	tableIndexes  map[string][]*catalogRelation

	constraints     []*catalogConstraint
	constraintByOID map[int64]*catalogConstraint

	rows map[string][]storage.Row

	// Preallocated OIDs keep every catalog relation deterministic regardless of
	// the order in which row sets are materialized.
	attrdefOIDs map[string]int64
	databaseOID int64
	procOIDs    map[string]int64

	oidOwners map[int64]string
}

// catalogSnapshotForQuery builds (once per statement) and returns the catalog
// snapshot the statement observes.
func (e *Executor) catalogSnapshotForQuery() (*catalogSnapshot, error) {
	if e.schema == nil || e.session == nil {
		return nil, pgerror.Unsupported("catalog introspection is not available in a stored expression")
	}
	if e.pgCatalog != nil {
		return e.pgCatalog, nil
	}
	snapshot, err := buildCatalogSnapshot(e)
	if err != nil {
		return nil, err
	}
	e.pgCatalog = snapshot
	return snapshot, nil
}

// visibleCatalogColumns drops the engine's hidden synthetic rowid column, which
// is not a user-visible PostgreSQL attribute.
func visibleCatalogColumns(schema *storage.Schema) []storage.Column {
	columns := make([]storage.Column, 0, len(schema.Columns))
	for _, column := range schema.Columns {
		if schema.UsesSyntheticRowID() && column.Name == "_rowid_" {
			continue
		}
		columns = append(columns, column)
	}
	return columns
}

func indexCatalogColumns(index *storage.Index) []storage.Column {
	columns := make([]storage.Column, len(index.Columns))
	for i, column := range index.Columns {
		name := column.Name
		if name == "" {
			name = fmt.Sprintf("expr%d", i+1)
		}
		columns[i] = storage.Column{Name: name, Type: "TEXT", Nullable: true}
	}
	return columns
}

func relationNamespaceOID(schemaName string) int64 {
	switch schemaName {
	case pgCatalogSchemaName:
		return pgCatalogNamespaceOID
	case informationSchemaName:
		return infoSchemaNamespaceOID
	default:
		return publicNamespaceOID
	}
}

// uniqueCatalogName returns base, or base with a numeric suffix, so a synthetic
// relation name never collides with a durable table or index name.
func uniqueCatalogName(base string, used map[string]bool) string {
	name := base
	for suffix := 2; used[strings.ToLower(name)]; suffix++ {
		name = fmt.Sprintf("%s%d", base, suffix)
	}
	used[strings.ToLower(name)] = true
	return name
}

func buildCatalogSnapshot(e *Executor) (*catalogSnapshot, error) {
	snapshot := &catalogSnapshot{
		database:        e.schema.GetDatabaseName(),
		relationByKey:   make(map[string]*catalogRelation),
		relationByOID:   make(map[int64]*catalogRelation),
		tableByName:     make(map[string]*catalogRelation),
		tableIndexes:    make(map[string][]*catalogRelation),
		constraintByOID: make(map[int64]*catalogConstraint),
		rows:            make(map[string][]storage.Row),
		attrdefOIDs:     make(map[string]int64),
		procOIDs:        make(map[string]int64),
		oidOwners:       make(map[int64]string),
	}
	if snapshot.database == "" {
		snapshot.database = "pizzasql"
	}

	tables, err := e.session.ListTables()
	if err != nil {
		return nil, err
	}
	sort.Strings(tables)

	indexNames, err := e.session.ListIndexes()
	if err != nil {
		return nil, err
	}
	sort.Strings(indexNames)

	var descriptors []*catalogRelation
	// usedNames guards every synthetic name against the durable names it could
	// collide with. A table named <x>_pkey shadows the synthetic index name a
	// PRIMARY KEY on x would otherwise take.
	usedNames := make(map[string]bool, len(tables)+len(indexNames)+len(e.views))
	tableSchemas := make(map[string]*storage.Schema, len(tables))
	for _, tableName := range tables {
		schema, err := e.session.GetSchema(tableName)
		if err != nil {
			return nil, fmt.Errorf("pg_catalog: resolve table %q: %w", tableName, err)
		}
		tableSchemas[tableName] = schema
		usedNames[strings.ToLower(schema.Name)] = true
		descriptors = append(descriptors, &catalogRelation{
			schemaName: publicSchemaName,
			name:       schema.Name,
			kind:       'r',
			columns:    visibleCatalogColumns(schema),
			table:      schema,
		})
	}

	for _, indexName := range indexNames {
		index, err := e.session.GetIndex(indexName)
		if err != nil {
			return nil, fmt.Errorf("pg_catalog: resolve index %q: %w", indexName, err)
		}
		usedNames[strings.ToLower(index.Name)] = true
		descriptors = append(descriptors, &catalogRelation{
			schemaName: publicSchemaName,
			name:       index.Name,
			kind:       'i',
			columns:    indexCatalogColumns(index),
			index:      index,
		})
	}

	// Session views are part of the schema the connection observes, so they are
	// reported the same way PostgreSQL reports a view: a pg_class row with
	// relkind 'v' plus matching pg_attribute and information_schema.columns rows.
	viewNames := sortedViewNames(e.views)
	for _, viewName := range viewNames {
		columns := e.viewColumns[viewName]
		usedNames[viewName] = true
		descriptors = append(descriptors, &catalogRelation{
			schemaName: publicSchemaName,
			name:       viewName,
			kind:       'v',
			columns:    columns,
		})
	}

	// One synthetic index per primary key so pg_index/pg_constraint have a
	// stable conindid. Composite keys keep their declared order.
	for _, tableName := range tables {
		schema := tableSchemas[tableName]
		// A table without a declared PRIMARY KEY is addressed by the engine's
		// synthetic _rowid_ column, which is not a PostgreSQL attribute and has
		// no index.
		if schema.UsesSyntheticRowID() {
			continue
		}
		keyColumns := schema.PrimaryKeyList()
		if len(keyColumns) == 0 {
			continue
		}
		index := &storage.Index{Name: uniqueCatalogName(schema.Name+"_pkey", usedNames), Table: schema.Name, Unique: true}
		for _, column := range keyColumns {
			index.Columns = append(index.Columns, storage.IndexColumn{Name: column})
		}
		descriptors = append(descriptors, &catalogRelation{
			schemaName:   publicSchemaName,
			name:         index.Name,
			kind:         'i',
			columns:      indexCatalogColumns(index),
			index:        index,
			primaryIndex: true,
		})
	}

	for _, def := range pgCatalogRelationDefs {
		descriptors = append(descriptors, &catalogRelation{
			schemaName: def.schema,
			name:       def.name,
			kind:       'v',
			columns:    pgCatalogRelationIndex[def.schema+"."+def.name].schema.Columns,
			virtual:    true,
		})
	}
	for _, def := range informationSchemaRelationDefs {
		descriptors = append(descriptors, &catalogRelation{
			schemaName: def.schema,
			name:       def.name,
			kind:       'v',
			columns:    pgCatalogRelationIndex[def.schema+"."+def.name].schema.Columns,
			virtual:    true,
		})
	}

	sort.SliceStable(descriptors, func(i, j int) bool {
		if descriptors[i].schemaName != descriptors[j].schemaName {
			return descriptors[i].schemaName < descriptors[j].schemaName
		}
		if descriptors[i].name != descriptors[j].name {
			return descriptors[i].name < descriptors[j].name
		}
		return descriptors[i].kind < descriptors[j].kind
	})

	for _, relation := range descriptors {
		relation.oid = snapshot.allocateOID("relation", catalogRelationKey(relation))
		snapshot.relations = append(snapshot.relations, relation)
		snapshot.relationByKey[catalogRelationKey(relation)] = relation
		snapshot.relationByOID[relation.oid] = relation
		if relation.kind == 'r' {
			snapshot.tableByName[strings.ToLower(relation.name)] = relation
		}
		if relation.kind == 'i' {
			tableKey := strings.ToLower(relation.index.Table)
			snapshot.tableIndexes[tableKey] = append(snapshot.tableIndexes[tableKey], relation)
		}
	}

	// Constraints are allocated after relations so conindid can point at the
	// durable or synthetic index OID.
	for _, relation := range snapshot.relations {
		if relation.kind != 'r' {
			continue
		}
		usedConstraintNames := make(map[string]bool)
		addConstraint := func(constraint *catalogConstraint) {
			constraint.table = relation
			constraint.oid = snapshot.allocateOID("constraint", relation.name+"."+constraint.name)
			snapshot.constraints = append(snapshot.constraints, constraint)
			snapshot.constraintByOID[constraint.oid] = constraint
		}

		for _, indexRelation := range snapshot.tableIndexes[strings.ToLower(relation.name)] {
			if !indexRelation.primaryIndex {
				continue
			}
			usedConstraintNames[strings.ToLower(indexRelation.name)] = true
			addConstraint(&catalogConstraint{
				name:     indexRelation.name,
				contype:  'p',
				indexOID: indexRelation.oid,
				columns:  indexColumnNames(indexRelation.index),
			})
		}
		for _, indexRelation := range snapshot.tableIndexes[strings.ToLower(relation.name)] {
			if indexRelation.primaryIndex || !indexRelation.index.Unique {
				continue
			}
			if usedConstraintNames[strings.ToLower(indexRelation.name)] {
				continue
			}
			usedConstraintNames[strings.ToLower(indexRelation.name)] = true
			addConstraint(&catalogConstraint{
				name:     indexRelation.name,
				contype:  'u',
				indexOID: indexRelation.oid,
				columns:  indexColumnNames(indexRelation.index),
			})
		}
		for i := range relation.table.ForeignKeys {
			fk := &relation.table.ForeignKeys[i]
			name := fk.Name
			if name == "" {
				name = relation.name + "_" + strings.Join(fk.Columns, "_") + "_fkey"
			}
			name = uniqueCatalogName(name, usedConstraintNames)
			addConstraint(&catalogConstraint{name: name, contype: 'f', columns: fk.Columns, fk: fk})
		}
		for i := range relation.table.Checks {
			check := &relation.table.Checks[i]
			name := check.Name
			if name == "" {
				name = relation.name + "_check"
			}
			name = uniqueCatalogName(name, usedConstraintNames)
			addConstraint(&catalogConstraint{name: name, contype: 'c', check: check})
		}
	}

	// Default expressions, the database row, and function rows are allocated in
	// sorted/declared order so their OIDs do not depend on which relation a
	// query reads first.
	for _, relation := range snapshot.relations {
		if relation.kind != 'r' {
			continue
		}
		for _, column := range relation.columns {
			if column.DefaultLiteral() == nil {
				continue
			}
			snapshot.attrdefOIDs[attrdefKey(relation.name, column.Name)] = snapshot.allocateOID("attrdef", relation.name+"."+column.Name)
		}
	}
	snapshot.databaseOID = snapshot.allocateOID("database", snapshot.database)
	for _, function := range pgCatalogFunctions {
		snapshot.procOIDs[function.name] = snapshot.allocateOID("proc", function.name)
	}

	return snapshot, nil
}

// attrdefKey identifies a column default in the snapshot's OID table.
func attrdefKey(table, column string) string {
	return strings.ToLower(table) + "." + strings.ToLower(column)
}

// sortedViewNames returns the session's view names in deterministic order.
func sortedViewNames(views map[string]*parser.SelectStmt) []string {
	names := make([]string, 0, len(views))
	for name := range views {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func indexColumnNames(index *storage.Index) []string {
	names := make([]string, len(index.Columns))
	for i, column := range index.Columns {
		names[i] = column.Name
	}
	return names
}

// regclassOID resolves a relation name the way a regclass literal does:
// an explicit schema or the public search path, with pg_catalog implicit.
func (s *catalogSnapshot) regclassOID(name string) (int64, bool) {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, `"`)
	if name == "" {
		return 0, false
	}
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		schemaName := strings.Trim(strings.TrimSpace(name[:dot]), `"`)
		tableName := strings.Trim(strings.TrimSpace(name[dot+1:]), `"`)
		if relation := s.regclassRelation(schemaName, tableName); relation != nil {
			return relation.oid, true
		}
		return 0, false
	}
	if relation := s.regclassRelation(publicSchemaName, name); relation != nil {
		return relation.oid, true
	}
	if relation := s.regclassRelation(pgCatalogSchemaName, name); relation != nil {
		return relation.oid, true
	}
	return 0, false
}

// regclassRelation resolves a schema-qualified relation name to the row a
// regclass literal names. PostgreSQL keeps relation names unique inside a
// schema, but the engine lets an index share its table's name, so the lookup
// prefers a table, then a session view, then an index.
func (s *catalogSnapshot) regclassRelation(schemaName, name string) *catalogRelation {
	key := strings.ToLower(schemaName + "." + name)
	for _, kind := range []byte{'r', 'v', 'i'} {
		if relation, ok := s.relationByKey[key+"#"+string(kind)]; ok {
			return relation
		}
	}
	return nil
}

// regclassName renders a relation OID the way regclass output does.
func (s *catalogSnapshot) regclassName(oid int64) (string, bool) {
	relation, ok := s.relationByOID[oid]
	if !ok {
		return "", false
	}
	return relation.name, true
}

// tableRelationFor resolves a table name (unqualified or schema-qualified) to
// its public relation row.
func (s *catalogSnapshot) tableRelationFor(name string) *catalogRelation {
	name = strings.TrimSpace(name)
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		name = name[dot+1:]
	}
	return s.tableByName[strings.ToLower(name)]
}

func catalogAttributeNumbers(schema *storage.Schema) map[string]int64 {
	numbers := make(map[string]int64, len(schema.Columns))
	position := int64(0)
	for _, column := range visibleCatalogColumns(schema) {
		position++
		numbers[strings.ToLower(column.Name)] = position
	}
	return numbers
}

func oidListText(values []int64) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.FormatInt(value, 10)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func defaultExpressionText(column storage.Column) interface{} {
	value := column.DefaultLiteral()
	if value == nil {
		return nil
	}
	return sqlLiteral(value)
}

// ---------------------------------------------------------------------------
// Row builders

func (s *catalogSnapshot) relationRows(key string) []storage.Row {
	key = strings.ToLower(key)
	if rows, ok := s.rows[key]; ok {
		return rows
	}
	var rows []storage.Row
	switch key {
	case "pg_catalog.pg_namespace":
		rows = s.pgNamespaceRows()
	case "pg_catalog.pg_class":
		rows = s.pgClassRows()
	case "pg_catalog.pg_attribute":
		rows = s.pgAttributeRows()
	case "pg_catalog.pg_type":
		rows = s.pgTypeRows()
	case "pg_catalog.pg_index":
		rows = s.pgIndexRows()
	case "pg_catalog.pg_constraint":
		rows = s.pgConstraintRows()
	case "pg_catalog.pg_attrdef":
		rows = s.pgAttrdefRows()
	case "pg_catalog.pg_database":
		rows = s.pgDatabaseRows()
	case "pg_catalog.pg_tablespace":
		rows = s.pgTablespaceRows()
	case "pg_catalog.pg_proc":
		rows = s.pgProcRows()
	case "pg_catalog.pg_am":
		rows = s.pgAmRows()
	case "pg_catalog.pg_description":
		rows = []storage.Row{}
	case "pg_catalog.pg_tables":
		rows = s.pgTablesRows()
	case "pg_catalog.pg_indexes":
		rows = s.pgIndexesRows()
	case "information_schema.tables":
		rows = s.informationSchemaTablesRows()
	case "information_schema.columns":
		rows = s.informationSchemaColumnsRows()
	case "information_schema.table_constraints":
		rows = s.informationSchemaTableConstraintsRows()
	case "information_schema.key_column_usage":
		rows = s.informationSchemaKeyColumnUsageRows()
	case "information_schema.referential_constraints":
		rows = s.informationSchemaReferentialConstraintsRows()
	case "information_schema.constraint_column_usage":
		rows = s.informationSchemaConstraintColumnUsageRows()
	case "information_schema.schemata":
		rows = s.informationSchemaSchemataRows()
	case "information_schema.sequences":
		rows = []storage.Row{}
	case "information_schema.views":
		rows = s.informationSchemaViewsRows()
	default:
		rows = []storage.Row{}
	}
	s.rows[key] = rows
	return rows
}

func (s *catalogSnapshot) pgNamespaceRows() []storage.Row {
	return []storage.Row{
		{"oid": pgCatalogNamespaceOID, "nspname": pgCatalogSchemaName, "nspowner": catalogOwnerOID, "nspacl": nil},
		{"oid": publicNamespaceOID, "nspname": publicSchemaName, "nspowner": catalogOwnerOID, "nspacl": nil},
		{"oid": infoSchemaNamespaceOID, "nspname": informationSchemaName, "nspowner": catalogOwnerOID, "nspacl": nil},
	}
}

func (s *catalogSnapshot) pgClassRows() []storage.Row {
	rows := make([]storage.Row, 0, len(s.relations))
	for _, relation := range s.relations {
		hasIndex := false
		checks := int64(0)
		relfilenode := int64(0)
		relam := int64(0)
		switch relation.kind {
		case 'r':
			hasIndex = len(s.tableIndexes[strings.ToLower(relation.name)]) > 0
			checks = int64(len(relation.table.Checks))
			relfilenode = relation.oid
		case 'i':
			relfilenode = relation.oid
			relam = pgBtreeAMOID
		}
		rows = append(rows, storage.Row{
			"oid":                 relation.oid,
			"relname":             relation.name,
			"relnamespace":        relationNamespaceOID(relation.schemaName),
			"reltype":             int64(0),
			"reloftype":           int64(0),
			"relowner":            catalogOwnerOID,
			"relam":               relam,
			"relfilenode":         relfilenode,
			"reltablespace":       int64(0),
			"relpages":            int64(0),
			"reltuples":           float64(0),
			"relallvisible":       int64(0),
			"reltoastrelid":       int64(0),
			"relhasindex":         hasIndex,
			"relisshared":         false,
			"relpersistence":      "p",
			"relkind":             string(relation.kind),
			"relnatts":            int64(len(relation.columns)),
			"relchecks":           checks,
			"relhasrules":         false,
			"relhastriggers":      false,
			"relhassubclass":      false,
			"relrowsecurity":      false,
			"relforcerowsecurity": false,
			"relispopulated":      true,
			"relreplident":        "d",
			"relispartition":      false,
			"relrewrite":          int64(0),
			"relfrozenxid":        int64(0),
			"relminmxid":          int64(0),
			"relacl":              nil,
			"reloptions":          nil,
			"relpartbound":        nil,
		})
	}
	return rows
}

func (s *catalogSnapshot) relationAttributeType(relation *catalogRelation, column storage.Column) pgBuiltinType {
	if relation.kind == 'i' && relation.index != nil {
		if table := s.tableRelationFor(relation.index.Table); table != nil {
			for _, tableColumn := range table.columns {
				if strings.EqualFold(tableColumn.Name, column.Name) {
					return pgTypeForDeclaredName(tableColumn.Type)
				}
			}
		}
	}
	return pgTypeForDeclaredName(column.Type)
}

func (s *catalogSnapshot) pgAttributeRows() []storage.Row {
	var rows []storage.Row
	for _, relation := range s.relations {
		for i, column := range relation.columns {
			builtin := s.relationAttributeType(relation, column)
			notNull := false
			hasDefault := false
			generated := ""
			if relation.kind == 'r' {
				notNull = !column.Nullable
				hasDefault = column.DefaultLiteral() != nil
				if column.GeneratedExpr != "" {
					generated = "s"
				}
			}
			arrayDimensions := int64(0)
			if builtin.isArray() {
				arrayDimensions = 1
			}
			rows = append(rows, storage.Row{
				"attrelid":       relation.oid,
				"attname":        column.Name,
				"atttypid":       builtin.oid,
				"attstattarget":  int64(-1),
				"attlen":         builtin.length,
				"attnum":         int64(i + 1),
				"attndims":       arrayDimensions,
				"attcacheoff":    int64(-1),
				"atttypmod":      int64(-1),
				"attbyval":       builtin.byval,
				"attalign":       builtin.align,
				"attstorage":     builtin.storage,
				"attcompression": "",
				"attnotnull":     notNull,
				"atthasdef":      hasDefault,
				"atthasmissing":  false,
				"attidentity":    "",
				"attgenerated":   generated,
				"attisdropped":   false,
				"attislocal":     true,
				"attinhcount":    int64(0),
				"attcollation":   int64(0),
				"attacl":         nil,
				"attoptions":     nil,
				"attfdwoptions":  nil,
				"attmissingval":  nil,
			})
		}
	}
	return rows
}

func (s *catalogSnapshot) pgTypeRows() []storage.Row {
	rows := make([]storage.Row, 0, len(pgBuiltinTypes))
	for _, t := range pgBuiltinTypes {
		rows = append(rows, storage.Row{
			"oid":            t.oid,
			"typname":        t.name,
			"typnamespace":   pgCatalogNamespaceOID,
			"typowner":       catalogOwnerOID,
			"typlen":         t.length,
			"typbyval":       t.byval,
			"typtype":        "b",
			"typcategory":    t.category,
			"typispreferred": false,
			"typisdefined":   true,
			"typdelim":       t.delim,
			"typrelid":       int64(0),
			"typsubscript":   nil,
			"typelem":        t.elem,
			"typarray":       t.array,
			"typinput":       int64(0),
			"typoutput":      int64(0),
			"typreceive":     int64(0),
			"typsend":        int64(0),
			"typmodin":       int64(0),
			"typmodout":      int64(0),
			"typanalyze":     int64(0),
			"typalign":       t.align,
			"typstorage":     t.storage,
			"typnotnull":     false,
			"typbasetype":    int64(0),
			"typtypmod":      int64(-1),
			"typndims":       int64(0),
			"typcollation":   int64(0),
			"typdefaultbin":  nil,
			"typdefault":     nil,
			"typacl":         nil,
		})
	}
	return rows
}

func (s *catalogSnapshot) indexAttributeNumbers(indexRelation *catalogRelation) []int64 {
	numbers := make([]int64, len(indexRelation.index.Columns))
	table := s.tableRelationFor(indexRelation.index.Table)
	if table == nil {
		return numbers
	}
	attnums := catalogAttributeNumbers(table.table)
	for i, column := range indexRelation.index.Columns {
		if column.Name == "" {
			continue
		}
		numbers[i] = attnums[strings.ToLower(column.Name)]
	}
	return numbers
}

func (s *catalogSnapshot) pgIndexRows() []storage.Row {
	var rows []storage.Row
	for _, relation := range s.relations {
		if relation.kind != 'i' || relation.index == nil {
			continue
		}
		table := s.tableRelationFor(relation.index.Table)
		tableOID := int64(0)
		if table != nil {
			tableOID = table.oid
		}
		attnums := s.indexAttributeNumbers(relation)
		options := make([]int64, len(attnums))
		var expressions []string
		hasExpression := false
		for _, column := range relation.index.Columns {
			if column.Expression != "" {
				expressions = append(expressions, column.Expression)
				hasExpression = true
			}
		}
		var indexprs interface{}
		if hasExpression {
			indexprs = strings.Join(expressions, ", ")
		}
		rows = append(rows, storage.Row{
			"indexrelid":     relation.oid,
			"indrelid":       tableOID,
			"indnatts":       int64(len(attnums)),
			"indnkeyatts":    int64(len(attnums)),
			"indisunique":    relation.index.Unique,
			"indisprimary":   relation.primaryIndex,
			"indisexclusion": false,
			"indimmediate":   true,
			"indisclustered": false,
			"indisvalid":     true,
			"indcheckxmin":   false,
			"indisready":     true,
			"indislive":      true,
			"indisreplident": false,
			"indkey":         oidListText(attnums),
			"indcollation":   oidListText(make([]int64, len(attnums))),
			"indclass":       oidListText(make([]int64, len(attnums))),
			"indoption":      oidListText(options),
			"indexprs":       indexprs,
			"indpred":        nil,
		})
	}
	return rows
}

func constraintActionCode(action string) string {
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case "CASCADE":
		return "c"
	case "RESTRICT":
		return "r"
	case "SET NULL":
		return "n"
	case "SET DEFAULT":
		return "d"
	default:
		return "a"
	}
}

func (s *catalogSnapshot) pgConstraintRows() []storage.Row {
	rows := make([]storage.Row, 0, len(s.constraints))
	for _, constraint := range s.constraints {
		var conkey interface{}
		if len(constraint.columns) > 0 {
			attnums := catalogAttributeNumbers(constraint.table.table)
			key := make([]int64, 0, len(constraint.columns))
			for _, name := range constraint.columns {
				key = append(key, attnums[strings.ToLower(name)])
			}
			conkey = oidListText(key)
		}

		var confrelid, confupdtype, confdeltype, confmatchtype, confkey interface{}
		if constraint.fk != nil {
			if parent := s.tableRelationFor(constraint.fk.RefTable); parent != nil {
				confrelid = parent.oid
				parentAttnums := catalogAttributeNumbers(parent.table)
				key := make([]int64, 0, len(constraint.fk.RefColumns))
				for _, name := range constraint.fk.RefColumns {
					key = append(key, parentAttnums[strings.ToLower(name)])
				}
				confkey = oidListText(key)
			} else {
				confrelid = int64(0)
			}
			confupdtype = constraintActionCode(constraint.fk.OnUpdate)
			confdeltype = constraintActionCode(constraint.fk.OnDelete)
			confmatchtype = "s"
		}

		var conbin interface{}
		if constraint.check != nil {
			conbin = constraint.check.Expression
		}

		rows = append(rows, storage.Row{
			"oid":           constraint.oid,
			"conname":       constraint.name,
			"connamespace":  publicNamespaceOID,
			"contype":       string(constraint.contype),
			"condeferrable": false,
			"condeferred":   false,
			"convalidated":  true,
			"conrelid":      constraint.table.oid,
			"contypid":      int64(0),
			"conindid":      constraint.indexOID,
			"conparentid":   int64(0),
			"confrelid":     confrelid,
			"confupdtype":   confupdtype,
			"confdeltype":   confdeltype,
			"confmatchtype": confmatchtype,
			"conislocal":    true,
			"coninhcount":   int64(0),
			"connoinherit":  false,
			"conkey":        conkey,
			"confkey":       confkey,
			"conpfeqop":     nil,
			"conppeqop":     nil,
			"conffeqop":     nil,
			"conexclop":     nil,
			"conbin":        conbin,
			"consrc":        conbin,
		})
	}
	return rows
}

func (s *catalogSnapshot) pgAttrdefRows() []storage.Row {
	var rows []storage.Row
	for _, relation := range s.relations {
		if relation.kind != 'r' {
			continue
		}
		attnums := catalogAttributeNumbers(relation.table)
		for _, column := range relation.columns {
			text := defaultExpressionText(column)
			if text == nil {
				continue
			}
			adnum := attnums[strings.ToLower(column.Name)]
			rows = append(rows, storage.Row{
				"oid":     s.attrdefOIDs[attrdefKey(relation.name, column.Name)],
				"adrelid": relation.oid,
				"adnum":   adnum,
				"adbin":   text,
			})
		}
	}
	return rows
}

func (s *catalogSnapshot) pgDatabaseRows() []storage.Row {
	return []storage.Row{{
		"oid":            s.databaseOID,
		"datname":        s.database,
		"datdba":         catalogOwnerOID,
		"encoding":       int64(6), // UTF8
		"datlocprovider": "c",
		"datistemplate":  false,
		"datallowconn":   true,
		"datconnlimit":   int64(-1),
		"datlastsysoid":  int64(0),
		"datfrozenxid":   int64(0),
		"datminmxid":     int64(0),
		"dattablespace":  pgDefaultTablespaceOID,
		"datcollate":     "C",
		"datctype":       "C",
		"datlocale":      nil,
		"daticurules":    nil,
		"datcollversion": nil,
		"datacl":         nil,
	}}
}

func (s *catalogSnapshot) pgTablespaceRows() []storage.Row {
	return []storage.Row{
		{"oid": pgDefaultTablespaceOID, "spcname": "pg_default", "spcowner": catalogOwnerOID, "spcacl": nil, "spcoptions": nil},
		{"oid": pgGlobalTablespaceOID, "spcname": "pg_global", "spcowner": catalogOwnerOID, "spcacl": nil, "spcoptions": nil},
	}
}

// pgCatalogFunctions is the pg_proc surface the executor answers. It mirrors
// evalCatalogFunction; names not listed here never appear in pg_proc.
var pgCatalogFunctions = []struct {
	name    string
	nargs   int64
	rettype int64
}{
	{"current_database", 0, 19},
	{"current_schema", 0, 19},
	{"format_type", 2, 25},
	{"has_table_privilege", 3, 16},
	{"obj_description", 2, 25},
	{"col_description", 2, 25},
	{"pg_get_constraintdef", 2, 25},
	{"pg_get_expr", 3, 25},
	{"pg_get_indexdef", 3, 25},
	{"pg_get_serial_sequence", 2, 25},
	{"pg_get_userbyid", 1, 19},
	{"pg_table_is_visible", 1, 16},
	{"to_regclass", 1, 2205},
	{"to_regtype", 1, 2206},
}

func (s *catalogSnapshot) pgProcRows() []storage.Row {
	rows := make([]storage.Row, 0, len(pgCatalogFunctions))
	for _, function := range pgCatalogFunctions {
		rows = append(rows, storage.Row{
			"oid":             s.procOIDs[function.name],
			"proname":         function.name,
			"pronamespace":    pgCatalogNamespaceOID,
			"proowner":        catalogOwnerOID,
			"prolang":         int64(12), // internal
			"procost":         float64(1),
			"prorows":         float64(0),
			"provariadic":     int64(0),
			"prosupport":      nil,
			"prokind":         "f",
			"prosecdef":       false,
			"proleakproof":    false,
			"proisstrict":     true,
			"proretset":       false,
			"provolatile":     "s",
			"proparallel":     "s",
			"pronargs":        function.nargs,
			"pronargdefaults": int64(0),
			"prorettype":      function.rettype,
			"proargtypes":     "",
			"proallargtypes":  nil,
			"proargmodes":     nil,
			"proargnames":     nil,
			"proargdefaults":  nil,
			"protrftypes":     nil,
			"prosrc":          function.name,
			"probin":          nil,
			"prosqlbody":      nil,
			"proconfig":       nil,
			"proacl":          nil,
		})
	}
	return rows
}

func (s *catalogSnapshot) pgAmRows() []storage.Row {
	return []storage.Row{
		{"oid": pgHeapAMOID, "amname": "heap", "amhandler": int64(0), "amtype": "t"},
		{"oid": pgBtreeAMOID, "amname": "btree", "amhandler": int64(0), "amtype": "i"},
	}
}

func (s *catalogSnapshot) pgTablesRows() []storage.Row {
	var rows []storage.Row
	for _, relation := range s.relations {
		if relation.kind != 'r' {
			continue
		}
		hasIndex := len(s.tableIndexes[strings.ToLower(relation.name)]) > 0
		rows = append(rows, storage.Row{
			"schemaname":  relation.schemaName,
			"tablename":   relation.name,
			"tableowner":  "pizzasql",
			"tablespace":  nil,
			"hasindexes":  hasIndex,
			"hasrules":    false,
			"hastriggers": false,
			"rowsecurity": false,
		})
	}
	return rows
}

func (s *catalogSnapshot) pgIndexesRows() []storage.Row {
	var rows []storage.Row
	for _, relation := range s.relations {
		if relation.kind != 'i' || relation.index == nil {
			continue
		}
		rows = append(rows, storage.Row{
			"schemaname": relation.schemaName,
			"tablename":  relation.index.Table,
			"indexname":  relation.name,
			"tablespace": nil,
			"indexdef":   s.indexDefinition(relation.index),
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// information_schema row builders

func (s *catalogSnapshot) informationSchemaTablesRows() []storage.Row {
	var rows []storage.Row
	for _, relation := range s.relations {
		if relation.kind == 'i' {
			continue
		}
		tableType := "BASE TABLE"
		if relation.kind != 'r' {
			tableType = "VIEW"
		}
		insertable := "NO"
		if relation.kind == 'r' {
			insertable = "YES"
		}
		rows = append(rows, storage.Row{
			"table_catalog":                s.database,
			"table_schema":                 relation.schemaName,
			"table_name":                   relation.name,
			"table_type":                   tableType,
			"self_referencing_column_name": nil,
			"reference_generation":         nil,
			"user_defined_type_catalog":    nil,
			"user_defined_type_schema":     nil,
			"user_defined_type_name":       nil,
			"is_insertable_into":           insertable,
			"is_typed":                     "NO",
			"commit_action":                nil,
		})
	}
	return rows
}

func informationSchemaColumn(relation *catalogRelation, column storage.Column, position int) storage.Row {
	builtin := pgTypeForDeclaredName(column.Type)
	dataType := builtin.sqlName
	if builtin.isArray() {
		dataType = "ARRAY"
	}
	generated := "NEVER"
	generationExpression := interface{}(nil)
	if column.GeneratedExpr != "" {
		generated = "ALWAYS"
		generationExpression = column.GeneratedExpr
	}
	return storage.Row{
		"table_catalog":            "", // filled by the caller
		"table_schema":             relation.schemaName,
		"table_name":               relation.name,
		"column_name":              column.Name,
		"ordinal_position":         int64(position),
		"column_default":           defaultExpressionText(column),
		"is_nullable":              map[bool]string{true: "YES", false: "NO"}[column.Nullable],
		"data_type":                dataType,
		"character_maximum_length": nil,
		"character_octet_length":   nil,
		"numeric_precision":        nil,
		"numeric_precision_radix":  nil,
		"numeric_scale":            nil,
		"datetime_precision":       nil,
		"interval_type":            nil,
		"interval_precision":       nil,
		"character_set_catalog":    nil,
		"character_set_schema":     nil,
		"character_set_name":       nil,
		"collation_catalog":        nil,
		"collation_schema":         nil,
		"collation_name":           nil,
		"domain_catalog":           nil,
		"domain_schema":            nil,
		"domain_name":              nil,
		"udt_catalog":              "", // filled by the caller
		"udt_schema":               pgCatalogSchemaName,
		"udt_name":                 builtin.name,
		"scope_catalog":            nil,
		"scope_schema":             nil,
		"scope_name":               nil,
		"maximum_cardinality":      nil,
		"dtd_identifier":           strconv.Itoa(position),
		"is_self_referencing":      "NO",
		"is_identity":              "NO",
		"identity_generation":      nil,
		"identity_start":           nil,
		"identity_increment":       nil,
		"identity_maximum":         nil,
		"identity_minimum":         nil,
		"identity_cycle":           "NO",
		"is_generated":             generated,
		"generation_expression":    generationExpression,
		"is_updatable":             "YES",
	}
}

func (s *catalogSnapshot) informationSchemaColumnsRows() []storage.Row {
	var rows []storage.Row
	for _, relation := range s.relations {
		if relation.kind == 'i' || relation.virtual {
			continue
		}
		for i, column := range relation.columns {
			row := informationSchemaColumn(relation, column, i+1)
			row["table_catalog"] = s.database
			row["udt_catalog"] = s.database
			rows = append(rows, row)
		}
	}
	return rows
}

func constraintTypeName(contype byte) string {
	switch contype {
	case 'p':
		return "PRIMARY KEY"
	case 'u':
		return "UNIQUE"
	case 'f':
		return "FOREIGN KEY"
	case 'c':
		return "CHECK"
	default:
		return "UNKNOWN"
	}
}

func (s *catalogSnapshot) informationSchemaTableConstraintsRows() []storage.Row {
	rows := make([]storage.Row, 0, len(s.constraints))
	for _, constraint := range s.constraints {
		rows = append(rows, storage.Row{
			"constraint_catalog": s.database,
			"constraint_schema":  publicSchemaName,
			"constraint_name":    constraint.name,
			"table_catalog":      s.database,
			"table_schema":       publicSchemaName,
			"table_name":         constraint.table.name,
			"constraint_type":    constraintTypeName(constraint.contype),
			"is_deferrable":      "NO",
			"initially_deferred": "NO",
			"enforced":           "YES",
			"nulls_distinct":     nil,
		})
	}
	return rows
}

func (s *catalogSnapshot) informationSchemaKeyColumnUsageRows() []storage.Row {
	var rows []storage.Row
	for _, constraint := range s.constraints {
		var columns []string
		switch constraint.contype {
		case 'p', 'u':
			columns = constraint.columns
		case 'f':
			columns = constraint.fk.Columns
		default:
			continue
		}
		for i, column := range columns {
			var position interface{}
			if constraint.contype == 'f' {
				position = int64(i + 1)
			}
			rows = append(rows, storage.Row{
				"constraint_catalog":            s.database,
				"constraint_schema":             publicSchemaName,
				"constraint_name":               constraint.name,
				"table_catalog":                 s.database,
				"table_schema":                  publicSchemaName,
				"table_name":                    constraint.table.name,
				"column_name":                   column,
				"ordinal_position":              int64(i + 1),
				"position_in_unique_constraint": position,
			})
		}
	}
	return rows
}

func (s *catalogSnapshot) uniqueConstraintFor(parent *catalogRelation, columns []string) string {
	for _, constraint := range s.constraints {
		if constraint.table != parent || (constraint.contype != 'p' && constraint.contype != 'u') {
			continue
		}
		keyColumns := constraint.columns
		if constraint.contype == 'u' && constraint.indexOID != 0 {
			for _, indexRelation := range s.relations {
				if indexRelation.oid == constraint.indexOID && indexRelation.index != nil {
					keyColumns = indexColumnNames(indexRelation.index)
					break
				}
			}
		}
		if len(keyColumns) != len(columns) {
			continue
		}
		matches := true
		for i := range columns {
			if !strings.EqualFold(keyColumns[i], columns[i]) {
				matches = false
				break
			}
		}
		if matches {
			return constraint.name
		}
	}
	return ""
}

func referentialRule(action string) string {
	if strings.TrimSpace(action) == "" {
		return "NO ACTION"
	}
	return strings.ToUpper(strings.TrimSpace(action))
}

func (s *catalogSnapshot) informationSchemaReferentialConstraintsRows() []storage.Row {
	var rows []storage.Row
	for _, constraint := range s.constraints {
		if constraint.contype != 'f' || constraint.fk == nil {
			continue
		}
		parent := s.tableRelationFor(constraint.fk.RefTable)
		var uniqueName interface{}
		if parent != nil {
			if name := s.uniqueConstraintFor(parent, constraint.fk.RefColumns); name != "" {
				uniqueName = name
			}
		}
		rows = append(rows, storage.Row{
			"constraint_catalog":        s.database,
			"constraint_schema":         publicSchemaName,
			"constraint_name":           constraint.name,
			"unique_constraint_catalog": s.database,
			"unique_constraint_schema":  publicSchemaName,
			"unique_constraint_name":    uniqueName,
			"match_option":              "NONE",
			"update_rule":               referentialRule(constraint.fk.OnUpdate),
			"delete_rule":               referentialRule(constraint.fk.OnDelete),
			"is_deferrable":             "NO",
			"initially_deferred":        "NO",
			"enforced":                  "YES",
		})
	}
	return rows
}

func (s *catalogSnapshot) informationSchemaConstraintColumnUsageRows() []storage.Row {
	var rows []storage.Row
	for _, constraint := range s.constraints {
		switch constraint.contype {
		case 'f':
			if constraint.fk == nil {
				continue
			}
			for _, column := range constraint.fk.RefColumns {
				rows = append(rows, storage.Row{
					"table_catalog":      s.database,
					"table_schema":       publicSchemaName,
					"table_name":         constraint.fk.RefTable,
					"constraint_catalog": s.database,
					"constraint_schema":  publicSchemaName,
					"constraint_name":    constraint.name,
					"column_name":        column,
				})
			}
		case 'c':
			if constraint.check == nil {
				continue
			}
			expr, err := parser.ParseExpr(constraint.check.Expression)
			if err != nil {
				continue
			}
			for _, column := range collectColumnRefs(expr) {
				rows = append(rows, storage.Row{
					"table_catalog":      s.database,
					"table_schema":       publicSchemaName,
					"table_name":         constraint.table.name,
					"constraint_catalog": s.database,
					"constraint_schema":  publicSchemaName,
					"constraint_name":    constraint.name,
					"column_name":        column,
				})
			}
		}
	}
	return rows
}

func (s *catalogSnapshot) informationSchemaSchemataRows() []storage.Row {
	return []storage.Row{
		{"catalog_name": s.database, "schema_name": pgCatalogSchemaName, "schema_owner": "pizzasql", "default_character_set_catalog": nil, "default_character_set_schema": nil, "default_character_set_name": nil, "sql_path": nil},
		{"catalog_name": s.database, "schema_name": publicSchemaName, "schema_owner": "pizzasql", "default_character_set_catalog": nil, "default_character_set_schema": nil, "default_character_set_name": nil, "sql_path": nil},
		{"catalog_name": s.database, "schema_name": informationSchemaName, "schema_owner": "pizzasql", "default_character_set_catalog": nil, "default_character_set_schema": nil, "default_character_set_name": nil, "sql_path": nil},
	}
}

// catalogSchemaByName resolves an unqualified pg_catalog relation name that is
// not shadowed by a durable table or a CTE.
func (e *Executor) catalogSchemaByName(name string) (*storage.Schema, bool) {
	entry, ok := pgCatalogRelationIndex[pgCatalogSchemaName+"."+strings.ToLower(name)]
	if !ok || e.session.TableExists(name) || e.cteTableExists(name) {
		return nil, false
	}
	return entry.schema, true
}

func (s *catalogSnapshot) informationSchemaViewsRows() []storage.Row {
	// A session view is listed here exactly when it is listed in pg_class with
	// relkind 'v'. The engine does not retain the view's SQL text, so the
	// definition is NULL rather than invented.
	var rows []storage.Row
	for _, relation := range s.relations {
		if relation.kind != 'v' || relation.virtual {
			continue
		}
		rows = append(rows, storage.Row{
			"table_catalog":              s.database,
			"table_schema":               relation.schemaName,
			"table_name":                 relation.name,
			"view_definition":            nil,
			"check_option":               "NONE",
			"is_updatable":               "NO",
			"is_insertable_into":         "NO",
			"is_trigger_updatable":       "NO",
			"is_trigger_deletable":       "NO",
			"is_trigger_insertable_into": "NO",
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// Function/relation helpers used by the executor's expression evaluator

// indexDefinition renders CREATE INDEX statement text, used by pg_indexes and
// pg_get_indexdef.
func (s *catalogSnapshot) indexDefinition(index *storage.Index) string {
	var builder strings.Builder
	builder.WriteString("CREATE ")
	if index.Unique {
		builder.WriteString("UNIQUE ")
	}
	builder.WriteString("INDEX ")
	builder.WriteString(index.Name)
	builder.WriteString(" ON ")
	builder.WriteString(publicSchemaName)
	builder.WriteByte('.')
	builder.WriteString(index.Table)
	builder.WriteString(" USING btree (")
	for i, column := range index.Columns {
		if i > 0 {
			builder.WriteString(", ")
		}
		if column.Expression != "" {
			builder.WriteString("(")
			builder.WriteString(column.Expression)
			builder.WriteString(")")
			continue
		}
		builder.WriteString(column.Name)
		if column.Desc {
			builder.WriteString(" DESC")
		}
	}
	builder.WriteString(")")
	return builder.String()
}

func (s *catalogSnapshot) indexDefinitionForOID(oid int64) (string, bool) {
	relation, ok := s.relationByOID[oid]
	if !ok || relation.kind != 'i' || relation.index == nil {
		return "", false
	}
	return s.indexDefinition(relation.index), true
}

func (s *catalogSnapshot) constraintDefinition(constraint *catalogConstraint) string {
	switch constraint.contype {
	case 'p':
		return "PRIMARY KEY (" + strings.Join(constraint.columns, ", ") + ")"
	case 'u':
		return "UNIQUE (" + strings.Join(constraint.columns, ", ") + ")"
	case 'f':
		if constraint.fk == nil {
			return ""
		}
		clause := "FOREIGN KEY (" + strings.Join(constraint.fk.Columns, ", ") + ") REFERENCES " + constraint.fk.RefTable + "(" + strings.Join(constraint.fk.RefColumns, ", ") + ")"
		if constraint.fk.OnDelete != "" {
			clause += " ON DELETE " + strings.ToUpper(constraint.fk.OnDelete)
		}
		if constraint.fk.OnUpdate != "" {
			clause += " ON UPDATE " + strings.ToUpper(constraint.fk.OnUpdate)
		}
		return clause
	case 'c':
		if constraint.check == nil {
			return ""
		}
		return "CHECK ((" + constraint.check.Expression + "))"
	default:
		return ""
	}
}

func (s *catalogSnapshot) constraintDefinitionForOID(oid int64) (string, bool) {
	constraint, ok := s.constraintByOID[oid]
	if !ok {
		return "", false
	}
	return s.constraintDefinition(constraint), true
}

// formatType renders a type OID the way format_type() does. Precision modifiers
// are not persisted by the engine, so callers pass -1.
func (s *catalogSnapshot) formatType(oid int64, typmod int64) (string, bool) {
	builtin, ok := pgBuiltinTypeByOID[oid]
	if !ok {
		return "???", false
	}
	name := builtin.sqlName
	if typmod < 0 {
		return name, true
	}
	switch oid {
	case 1043, 1042: // varchar, bpchar
		return fmt.Sprintf("%s(%d)", name, typmod-4), true
	case 1700: // numeric
		modifier := typmod - 4
		precision := (modifier >> 16) & 0xffff
		scale := modifier & 0xffff
		return fmt.Sprintf("numeric(%d,%d)", precision, scale), true
	default:
		return name, true
	}
}

// typeOIDForName resolves a regtype literal.
func (s *catalogSnapshot) typeOIDForName(name string) (int64, bool) {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, `"`)
	upper := strings.ToUpper(name)
	if strings.HasSuffix(upper, "[]") || strings.HasSuffix(upper, " ARRAY") {
		elementName := strings.TrimSuffix(strings.TrimSuffix(upper, " ARRAY"), "[]")
		element := pgTypeForDeclaredName(elementName)
		if array, ok := pgBuiltinTypeByOID[element.array]; ok && element.array != 0 {
			return array.oid, true
		}
		return 0, false
	}
	if !knownDeclaredTypeName(upper) {
		if builtin, ok := pgBuiltinTypeByName[strings.ToLower(name)]; ok {
			return builtin.oid, true
		}
		return 0, false
	}
	return pgTypeForDeclaredName(upper).oid, true
}

// knownDeclaredTypeName reports whether a type spelling maps to a built-in.
func knownDeclaredTypeName(upper string) bool {
	switch upper {
	case "INTEGER", "INT", "INT4", "BIGINT", "INT8", "SMALLINT", "INT2", "TINYINT", "MEDIUMINT",
		"TEXT", "CLOB", "NCHAR", "NVARCHAR", "VARCHAR", "CHARACTER VARYING", "CHAR", "CHARACTER",
		"BOOLEAN", "BOOL", "REAL", "FLOAT", "FLOAT4", "DOUBLE", "DOUBLE PRECISION", "FLOAT8",
		"NUMERIC", "DECIMAL", "JSONB", "JSON", "BLOB", "BYTEA", "DATETIME", "TIMESTAMP",
		"TIMESTAMPTZ", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITHOUT TIME ZONE", "DATE",
		"TIME", "TIME WITHOUT TIME ZONE", "TIME WITH TIME ZONE", "UUID", "OID", "NAME",
		"REGCLASS", "REGTYPE":
		return true
	default:
		return false
	}
}

func (s *catalogSnapshot) typeNameForOID(oid int64) (string, bool) {
	builtin, ok := pgBuiltinTypeByOID[oid]
	if !ok {
		return "", false
	}
	return builtin.sqlName, true
}

func (s *catalogSnapshot) relationVisible(oid int64) bool {
	relation, ok := s.relationByOID[oid]
	if !ok {
		return false
	}
	switch relation.schemaName {
	case publicSchemaName, pgCatalogSchemaName, informationSchemaName:
		return true
	default:
		return false
	}
}

// numericArg converts a function argument that denotes an OID.
func numericArg(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case RegOID:
		return v.oid, true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// RegOID is the value of a regclass or regtype cast. Numeric contexts use the
// OID; text output renders the resolved name, matching PostgreSQL's symbolic
// type rendering.
type RegOID struct {
	oid  int64
	name string
}

// OID returns the numeric OID the value carries.
func (r RegOID) OID() int64 { return r.oid }

// String renders the symbolic name.
func (r RegOID) String() string { return r.name }

// evalCatalogFunction answers the PostgreSQL catalog-introspection builtins.
// handled is false for every other function so evalFunctionCall keeps them.
func (e *Executor) evalCatalogFunction(name string, fn *parser.FunctionCall, args []interface{}) (interface{}, bool, error) {
	switch name {
	case "CURRENT_SCHEMA":
		return e.currentSchemaName(), true, nil
	case "CURRENT_DATABASE":
		if e.schema == nil {
			return "pizzasql", true, nil
		}
		database := e.schema.GetDatabaseName()
		if database == "" {
			database = "pizzasql"
		}
		return database, true, nil
	case "PG_GET_SERIAL_SEQUENCE", "OBJ_DESCRIPTION", "COL_DESCRIPTION":
		// The engine has no sequences and stores no catalog comments.
		return nil, true, nil
	case "PG_GET_USERBYID":
		return "pizzasql", true, nil
	case "CURRENT_SETTING":
		if len(args) == 0 || args[0] == nil {
			return nil, true, nil
		}
		value, err := e.settingValue(toString(args[0]))
		if err != nil {
			return nil, true, err
		}
		return value, true, nil
	case "PG_GET_EXPR":
		if len(args) == 0 || args[0] == nil {
			return nil, true, nil
		}
		if text, ok := args[0].(string); ok {
			return text, true, nil
		}
		return toString(args[0]), true, nil
	}

	switch name {
	case "FORMAT_TYPE", "PG_TABLE_IS_VISIBLE", "PG_GET_INDEXDEF", "PG_GET_CONSTRAINTDEF", "HAS_TABLE_PRIVILEGE", "TO_REGCLASS", "TO_REGTYPE":
	default:
		return nil, false, nil
	}

	snapshot, err := e.catalogSnapshotForQuery()
	if err != nil {
		return nil, true, err
	}

	switch name {
	case "TO_REGCLASS", "TO_REGTYPE":
		// PostgreSQL's to_regclass/to_regtype return NULL for a name that does
		// not exist instead of failing the statement.
		if len(args) == 0 || args[0] == nil {
			return nil, true, nil
		}
		if reg, ok := args[0].(RegOID); ok {
			return reg, true, nil
		}
		text, ok := args[0].(string)
		if !ok {
			return nil, true, pgerror.InvalidText("%s requires a text argument", strings.ToLower(name))
		}
		var oid int64
		var found bool
		if name == "TO_REGTYPE" {
			oid, found = snapshot.typeOIDForName(text)
		} else {
			oid, found = snapshot.regclassOID(text)
		}
		if !found {
			return nil, true, nil
		}
		var resolved string
		if name == "TO_REGTYPE" {
			resolved, _ = snapshot.typeNameForOID(oid)
		} else {
			resolved, _ = snapshot.regclassName(oid)
		}
		return RegOID{oid: oid, name: resolved}, true, nil
	case "FORMAT_TYPE":
		if len(args) == 0 {
			return nil, true, pgerror.Unsupported("format_type requires a type OID")
		}
		oid, ok := numericArg(args[0])
		if !ok {
			return nil, true, pgerror.InvalidText("format_type requires a numeric type OID")
		}
		typmod := int64(-1)
		if len(args) > 1 && args[1] != nil {
			if value, ok := numericArg(args[1]); ok {
				typmod = value
			}
		}
		rendered, _ := snapshot.formatType(oid, typmod)
		return rendered, true, nil
	case "PG_TABLE_IS_VISIBLE":
		if len(args) == 0 || args[0] == nil {
			return nil, true, nil
		}
		oid, ok := numericArg(args[0])
		if !ok {
			return false, true, nil
		}
		return snapshot.relationVisible(oid), true, nil
	case "PG_GET_INDEXDEF":
		if len(args) == 0 || args[0] == nil {
			return nil, true, nil
		}
		oid, ok := numericArg(args[0])
		if !ok {
			return nil, true, pgerror.InvalidText("pg_get_indexdef requires an index OID")
		}
		definition, found := snapshot.indexDefinitionForOID(oid)
		if !found {
			return nil, true, nil
		}
		return definition, true, nil
	case "PG_GET_CONSTRAINTDEF":
		if len(args) == 0 || args[0] == nil {
			return nil, true, nil
		}
		oid, ok := numericArg(args[0])
		if !ok {
			return nil, true, pgerror.InvalidText("pg_get_constraintdef requires a constraint OID")
		}
		definition, found := snapshot.constraintDefinitionForOID(oid)
		if !found {
			return nil, true, nil
		}
		return definition, true, nil
	case "HAS_TABLE_PRIVILEGE":
		tableArg := interface{}(nil)
		switch len(args) {
		case 2:
			tableArg = args[0]
		case 3:
			tableArg = args[1]
		default:
			return false, true, nil
		}
		if tableArg == nil {
			return nil, true, nil
		}
		if oid, ok := numericArg(tableArg); ok {
			return snapshot.relationVisible(oid), true, nil
		}
		_, ok := snapshot.regclassOID(toString(tableArg))
		return ok, true, nil
	default:
		return nil, false, nil
	}
}

// currentSchemaName reports the first schema in the effective search path. The
// engine only materializes public, pg_catalog, and information_schema, so a
// search path naming anything else falls back to public.
func (e *Executor) currentSchemaName() string {
	path := e.effectiveSearchPath()
	if len(path) > 0 && path[0] != "" {
		return path[0]
	}
	return publicSchemaName
}

// catalogCast handles regclass/regtype casts and text casts of their values.
func (e *Executor) catalogCast(expr *parser.CastExpr, value interface{}) (interface{}, bool, error) {
	typeName := strings.ToUpper(strings.TrimSpace(expr.Type.Name))
	switch typeName {
	case "REGCLASS", "REGTYPE":
		snapshot, err := e.catalogSnapshotForQuery()
		if err != nil {
			return nil, true, err
		}
		if reg, ok := value.(RegOID); ok {
			return reg, true, nil
		}
		if text, ok := value.(string); ok {
			if typeName == "REGTYPE" {
				oid, found := snapshot.typeOIDForName(text)
				if !found {
					return nil, true, pgerror.UndefinedTable("type %q does not exist", text)
				}
				name, _ := snapshot.typeNameForOID(oid)
				return RegOID{oid: oid, name: name}, true, nil
			}
			oid, found := snapshot.regclassOID(text)
			if !found {
				return nil, true, pgerror.UndefinedTable("relation %q does not exist", text)
			}
			name, _ := snapshot.regclassName(oid)
			return RegOID{oid: oid, name: name}, true, nil
		}
		oid, ok := numericArg(value)
		if !ok {
			return nil, true, pgerror.InvalidText("cannot cast %T to %s", value, typeName)
		}
		var name string
		if typeName == "REGTYPE" {
			name, _ = snapshot.typeNameForOID(oid)
		} else {
			name, _ = snapshot.regclassName(oid)
		}
		if name == "" {
			name = strconv.FormatInt(oid, 10)
		}
		return RegOID{oid: oid, name: name}, true, nil
	case "TEXT", "VARCHAR", "CHAR", "CHARACTER", "CLOB", "NCHAR", "NVARCHAR":
		if reg, ok := value.(RegOID); ok {
			return reg.name, true, nil
		}
	case "OID":
		if reg, ok := value.(RegOID); ok {
			return reg.oid, true, nil
		}
	}
	return nil, false, nil
}

// isCatalogWriteTarget reports a DML/DDL statement aimed at a virtual relation.
func (e *Executor) rejectCatalogWriteTarget(stmt parser.Statement) error {
	var refs []parser.TableRef
	switch s := stmt.(type) {
	case *parser.InsertStmt:
		if s.Table != nil {
			refs = append(refs, *s.Table)
		}
	case *parser.UpdateStmt:
		if s.Table != nil {
			refs = append(refs, *s.Table)
		}
	case *parser.DeleteStmt:
		if s.Table != nil {
			refs = append(refs, *s.Table)
		}
	case *parser.CreateTableStmt:
		if s.Table != nil {
			refs = append(refs, *s.Table)
		}
	case *parser.CreateIndexStmt:
		refs = append(refs, parser.TableRef{Name: s.Table})
	case *parser.AlterTableStmt:
		refs = append(refs, parser.TableRef{Name: s.Table})
	case *parser.CreateViewStmt:
		if s.View != nil {
			refs = append(refs, *s.View)
		}
	case *parser.DropTableStmt:
		for _, table := range s.Tables {
			if table != nil {
				refs = append(refs, *table)
			}
		}
	}
	for _, ref := range refs {
		if _, ok := e.catalogRelationFor(ref); !ok {
			continue
		}
		schemaName := strings.ToLower(ref.Schema)
		if schemaName == "" || schemaName == publicSchemaName {
			schemaName = pgCatalogSchemaName
		}
		return pgerror.Unsupported("%s.%s is a read-only virtual relation; catalog relations are synthesized from the durable schema and cannot be written", schemaName, strings.ToLower(ref.Name))
	}
	return nil
}

// ---------------------------------------------------------------------------
// SELECT over virtual catalog relations

// catalogSelectRelation is the column metadata of one FROM entry.
type catalogSelectRelation struct {
	qualifier string
	columns   []storage.Column
}

// catalogFlatRefs flattens a FROM clause, including JOIN chains, preserving the
// schema qualifier of every entry.
func catalogFlatRefs(from []parser.TableRef) []parser.TableRef {
	var refs []parser.TableRef
	for _, ref := range from {
		cur := ref
		for {
			flat := cur
			flat.Join = nil
			flat.Subquery = cur.Subquery
			flat.Function = cur.Function
			refs = append(refs, flat)
			if cur.Join == nil || cur.Join.Table == nil {
				break
			}
			cur = *cur.Join.Table
		}
	}
	return refs
}

func catalogRefQualifier(ref parser.TableRef) string {
	if ref.Alias != "" {
		return ref.Alias
	}
	return ref.Name
}

// catalogSelectMetadata resolves the column list of every FROM relation so
// SELECT * and table.* expand, and so projection types stay accurate.
func (e *Executor) catalogSelectMetadata(from []parser.TableRef) ([]catalogSelectRelation, error) {
	var relations []catalogSelectRelation
	for _, ref := range catalogFlatRefs(from) {
		if ref.Subquery != nil || ref.Function != nil {
			continue
		}
		var columns []storage.Column
		if entry, ok := e.catalogRelationFor(ref); ok {
			columns = entry.schema.Columns
		} else if cte, ok := e.cteTableFor(ref.Name); ok {
			columns = schemaFromColumns(cte.columns).Columns
		} else {
			schema, err := e.session.GetSchema(ref.Name)
			if err != nil {
				return nil, err
			}
			columns = visibleCatalogColumns(schema)
		}
		relations = append(relations, catalogSelectRelation{qualifier: catalogRefQualifier(ref), columns: columns})
	}
	return relations, nil
}

// materializeCatalogTableRef reads one FROM entry. Catalog relations come from
// the statement's virtual snapshot; everything else goes through the ordinary
// materializer so a catalog query can still join a real table or read a
// derived table.
func (e *Executor) materializeCatalogTableRef(ref parser.TableRef) ([]storage.Row, error) {
	if entry, ok := e.catalogRelationFor(ref); ok {
		snapshot, err := e.catalogSnapshotForQuery()
		if err != nil {
			return nil, err
		}
		rows := cloneRows(snapshot.relationRows(entry.def.schema + "." + entry.def.name))
		alias := catalogRefQualifier(ref)
		for i := range rows {
			rows[i] = e.addTableAlias(rows[i], alias)
		}
		return rows, nil
	}
	if cte, ok := e.cteTableFor(ref.Name); ok {
		rows := cloneRows(cte.rows)
		alias := catalogRefQualifier(ref)
		for i := range rows {
			rows[i] = e.addTableAlias(rows[i], alias)
		}
		return rows, nil
	}
	return e.materializeTableRef(ref)
}

// materializeCatalogFrom materializes the whole FROM list, applying each
// entry's JOIN chain and cross-joining comma-separated entries.
func (e *Executor) materializeCatalogFrom(from []parser.TableRef) ([]storage.Row, error) {
	rows, err := e.materializeCatalogTableRef(from[0])
	if err != nil {
		return nil, err
	}
	if from[0].Join != nil {
		rows, err = e.executeJoins(from[0], rows)
		if err != nil {
			return nil, err
		}
	}
	for _, ref := range from[1:] {
		right, err := e.materializeCatalogTableRef(ref)
		if err != nil {
			return nil, err
		}
		if ref.Join != nil {
			right, err = e.executeJoins(ref, right)
			if err != nil {
				return nil, err
			}
		}
		joined := make([]storage.Row, 0, len(rows)*len(right))
		for _, leftRow := range rows {
			for _, rightRow := range right {
				combined := make(storage.Row, len(leftRow)+len(rightRow))
				for key, value := range leftRow {
					combined[key] = value
				}
				for key, value := range rightRow {
					combined[key] = value
				}
				joined = append(joined, combined)
			}
		}
		rows = joined
	}
	return rows, nil
}

// expandCatalogTableStars rewrites table.* projections into explicit column
// references so the shared materialized projection can read qualified keys.
func expandCatalogTableStars(stmt *parser.SelectStmt, relations []catalogSelectRelation) (*parser.SelectStmt, error) {
	expanded := *stmt
	if len(stmt.Columns) == 0 {
		return &expanded, nil
	}
	columns := make([]parser.SelectColumn, 0, len(stmt.Columns))
	for _, projection := range stmt.Columns {
		if projection.TableStar == "" {
			columns = append(columns, projection)
			continue
		}
		var matched *catalogSelectRelation
		for i := range relations {
			if strings.EqualFold(relations[i].qualifier, projection.TableStar) {
				matched = &relations[i]
				break
			}
		}
		if matched == nil {
			return nil, fmt.Errorf("cannot expand %s.*: relation columns are not available", projection.TableStar)
		}
		for _, column := range matched.columns {
			columns = append(columns, parser.SelectColumn{
				Expr:  &parser.ColumnRef{Table: matched.qualifier, Column: column.Name},
				Alias: column.Name,
			})
		}
	}
	expanded.Columns = columns
	return &expanded, nil
}

func catalogJoinKeyParts(key string) (string, string) {
	if dot := strings.LastIndexByte(key, '.'); dot >= 0 {
		return key[:dot], key[dot+1:]
	}
	return "", key
}

// catalogProjectionTypes computes result types parallel to the projection. The
// resolver follows starKeys, so both qualified and unqualified references keep
// the declared catalog type (OID, NAME, BOOL, ...) instead of falling back to
// TEXT over the wire.
func catalogProjectionTypes(stmt *parser.SelectStmt, starKeys, types []string) []string {
	resolve := func(table, column string) (string, bool) {
		for i, key := range starKeys {
			qualifier, name := catalogJoinKeyParts(key)
			if !strings.EqualFold(name, column) {
				continue
			}
			if table != "" && !strings.EqualFold(table, qualifier) {
				continue
			}
			if i < len(types) && types[i] != "" {
				return types[i], true
			}
			return "", true
		}
		return "", false
	}
	result := make([]string, 0, len(stmt.Columns))
	for _, projection := range stmt.Columns {
		if projection.Star {
			result = append(result, types...)
			continue
		}
		result = append(result, inferExprType(projection.Expr, resolve))
	}
	return result
}

// executeSelectOnCatalogRelations answers a SELECT whose FROM clause reads at
// least one virtual relation. Rows are materialized from the session's schema
// view, so staged transactional DDL is visible to the issuing connection only.
func (e *Executor) executeSelectOnCatalogRelations(stmt *parser.SelectStmt) (*Result, error) {
	relations, err := e.catalogSelectMetadata(stmt.From)
	if err != nil {
		return nil, err
	}
	expanded, err := expandCatalogTableStars(stmt, relations)
	if err != nil {
		return nil, err
	}

	rows, err := e.materializeCatalogFrom(stmt.From)
	if err != nil {
		return nil, err
	}

	var columns, starKeys, types, qualifiers []string
	for _, relation := range relations {
		for _, column := range relation.columns {
			columns = append(columns, column.Name)
			starKeys = append(starKeys, relation.qualifier+"."+column.Name)
			types = append(types, column.Type)
			qualifiers = append(qualifiers, relation.qualifier)
		}
	}

	// The JOIN chain is already applied by materializeCatalogFrom; hand the
	// shared materialized path rows without a pending join.
	tail := *expanded
	tailFrom := make([]parser.TableRef, len(expanded.From))
	copy(tailFrom, expanded.From)
	tailFrom[0].Join = nil
	tail.From = tailFrom

	e.pushTypeScope(materializedTypeResolver(columns, types, qualifiers))
	result, err := e.executeSelectOnMaterializedRows(&tail, columns, starKeys, rows)
	e.popTypeScope()
	if err != nil {
		return nil, err
	}
	result.ColumnTypes = catalogProjectionTypes(&tail, starKeys, types)
	if tail.Distinct {
		result.Rows = e.applyDistinct(result.Rows)
	}
	return result, nil
}
