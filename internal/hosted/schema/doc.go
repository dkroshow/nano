// Package schema defines the canonical control-store table schemas and
// migration DDL for hosted permission evidence and native-recovery records.
//
// It does not open databases, order migrations or own record storage.
// Database access and publication belong to state/db and state/store;
// edge/statewiring orders migrations, and hosted/store operates the permission evidence records.
package schema
