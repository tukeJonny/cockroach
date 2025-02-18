// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// Package descs provides abstractions for dealing with sets of descriptors.
// It is utilized during schema changes and by catalog.Accessor implementations.
package descs

import (
	"bytes"
	"context"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkeys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/hydratedtables"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/lease"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// MakeCollection constructs a Collection.
func MakeCollection(
	leaseMgr *lease.Manager,
	settings *cluster.Settings,
	sessionData *sessiondata.SessionData,
	hydratedTables *hydratedtables.Cache,
	virtualSchemas catalog.VirtualSchemas,
) Collection {

	// Allow a nil leaseMgr for testing.
	codec := keys.SystemSQLCodec
	if leaseMgr != nil {
		codec = leaseMgr.Codec()
	}
	return Collection{
		leaseMgr:       leaseMgr,
		settings:       settings,
		sessionData:    sessionData,
		hydratedTables: hydratedTables,
		virtualSchemas: virtualSchemas,
		leased:         makeLeasedDescriptors(leaseMgr),
		synthetic:      makeSyntheticDescriptors(),
		kv:             makeReadDescriptors(codec),
	}
}

// NewCollection constructs a new *Collection.
func NewCollection(
	settings *cluster.Settings,
	leaseMgr *lease.Manager,
	hydratedTables *hydratedtables.Cache,
	virtualSchemas catalog.VirtualSchemas,
) *Collection {
	tc := MakeCollection(leaseMgr, settings, nil, hydratedTables, virtualSchemas)
	return &tc
}

// Collection is a collection of descriptors held by a single session that
// serves SQL requests, or a background job using descriptors. The
// collection is cleared using ReleaseAll() which is called at the
// end of each transaction on the session, or on hitting conditions such
// as errors, or retries that result in transaction timestamp changes.
type Collection struct {
	// leaseMgr manages acquiring and releasing per-descriptor leases.
	leaseMgr *lease.Manager
	// virtualSchemas optionally holds the virtual schemas.
	virtualSchemas catalog.VirtualSchemas

	// A collection of descriptors valid for the timestamp. They are released once
	// the transaction using them is complete.
	leased leasedDescriptors

	// A collection of descriptors which were read from the store.
	kv kvDescriptors

	// syntheticDescriptors contains in-memory descriptors which override all
	// other matching descriptors during immutable descriptor resolution (by name
	// or by ID), but should not be written to disk. These support internal
	// queries which need to use a special modified descriptor (e.g. validating
	// non-public schema elements during a schema change). Attempting to resolve
	// a mutable descriptor by name or ID when a matching synthetic descriptor
	// exists is illegal.
	synthetic syntheticDescriptors

	// settings dictate whether we validate descriptors on write.
	settings *cluster.Settings

	// sessionData is the SessionData of the current session, if this Collection
	// is being used in the context of a session. It is stored so that the Collection
	// knows about state of temporary schemas (name and ID) for resolution.
	sessionData *sessiondata.SessionData

	// hydratedTables is node-level cache of table descriptors which utlize
	// user-defined types.
	hydratedTables *hydratedtables.Cache

	// skipValidationOnWrite should only be set to true during forced descriptor
	// repairs.
	skipValidationOnWrite bool

	// droppedDescriptors that will not need to wait for new
	// lease versions.
	deletedDescs []catalog.Descriptor
}

var _ catalog.Accessor = (*Collection)(nil)

// MaybeUpdateDeadline updates the deadline in a given transaction
// based on the leased descriptors in this collection. This update is
// only done when a deadline exists.
func (tc *Collection) MaybeUpdateDeadline(ctx context.Context, txn *kv.Txn) (err error) {
	return tc.leased.maybeUpdateDeadline(ctx, txn)
}

func (tc *Collection) maybeGetVirtualObjectDesc(
	schema string, object string, flags tree.ObjectLookupFlags, db string,
) (isVirtual bool, _ catalog.Descriptor, _ error) {
	if tc.virtualSchemas == nil {
		return false, nil, nil
	}
	scEntry, ok := tc.virtualSchemas.GetVirtualSchema(schema)
	if !ok {
		return false, nil, nil
	}
	desc, err := scEntry.GetObjectByName(object, flags)
	if err != nil {
		return true, nil, err
	}
	if desc == nil {
		if flags.Required {
			obj := tree.NewQualifiedObjectName(db, schema, object, flags.DesiredObjectKind)
			return true, nil, sqlerrors.NewUndefinedObjectError(obj, flags.DesiredObjectKind)
		}
		return true, nil, nil
	}
	if flags.RequireMutable {
		return true, nil, catalog.NewMutableAccessToVirtualSchemaError(scEntry, object)
	}
	return true, desc.Desc(), nil
}

// SkipValidationOnWrite avoids validating uncommitted descriptors prior to
// a transaction commit.
func (tc *Collection) SkipValidationOnWrite() {
	tc.skipValidationOnWrite = true
}

// ReleaseSpecifiedLeases releases the leases for the descriptors with ids in
// the passed slice. Errors are logged but ignored.
func (tc *Collection) ReleaseSpecifiedLeases(ctx context.Context, descs []lease.IDVersion) {
	tc.leased.release(ctx, descs)
}

// ReleaseLeases releases all leases. Errors are logged but ignored.
func (tc *Collection) ReleaseLeases(ctx context.Context) {
	tc.leased.releaseAll(ctx)
}

// ReleaseAll releases all state currently held by the Collection.
// ReleaseAll calls ReleaseLeases.
func (tc *Collection) ReleaseAll(ctx context.Context) {
	tc.ReleaseLeases(ctx)
	tc.kv.reset()
	tc.synthetic.reset()
	tc.deletedDescs = nil
}

// HasUncommittedTables returns true if the Collection contains uncommitted
// tables.
func (tc *Collection) HasUncommittedTables() bool {
	return tc.kv.hasUncommittedTables()
}

// HasUncommittedTypes returns true if the Collection contains uncommitted
// types.
func (tc *Collection) HasUncommittedTypes() bool {
	return tc.kv.hasUncommittedTypes()
}

// Satisfy the linter.
var _ = (*Collection).HasUncommittedTypes

// AddUncommittedDescriptor adds an uncommitted descriptor modified in the
// transaction to the Collection. The descriptor must either be a new descriptor
// or carry the original version or carry the subsequent version to the original
// version.
//
// Subsequent attempts to resolve this descriptor mutably, either by name or ID
// will return this exact object. Subsequent attempts to resolve this descriptor
// immutably will return a copy of the descriptor in the current state. A deep
// copy is performed in this call.
func (tc *Collection) AddUncommittedDescriptor(desc catalog.MutableDescriptor) error {
	_, err := tc.kv.addUncommittedDescriptor(desc)
	return err
}

// maybeRefreshCachedFieldsOnTypeDescriptor refreshes the cached fields on a
// Mutable if the given descriptor is a type descriptor and works as a pass
// through for all other descriptors. Mutable type descriptors are refreshed to
// reconstruct enumMetadata. This ensures that tables hydration following a
// type descriptor update (in the same txn) happens using the modified fields.
func maybeRefreshCachedFieldsOnTypeDescriptor(
	desc catalog.MutableDescriptor,
) (catalog.MutableDescriptor, error) {
	typeDesc, ok := desc.(catalog.TypeDescriptor)
	if ok {
		return typedesc.UpdateCachedFieldsOnModifiedMutable(typeDesc)
	}
	return desc, nil
}

// ValidateOnWriteEnabled is the cluster setting used to enable or disable
// validating descriptors prior to writing.
var ValidateOnWriteEnabled = settings.RegisterBoolSetting(
	"sql.catalog.descs.validate_on_write.enabled",
	"set to true to validate descriptors prior to writing, false to disable; default is true",
	true, /* defaultValue */
)

// WriteDescToBatch calls MaybeIncrementVersion, adds the descriptor to the
// collection as an uncommitted descriptor, and writes it into b.
func (tc *Collection) WriteDescToBatch(
	ctx context.Context, kvTrace bool, desc catalog.MutableDescriptor, b *kv.Batch,
) error {
	desc.MaybeIncrementVersion()
	if !tc.skipValidationOnWrite && ValidateOnWriteEnabled.Get(&tc.settings.SV) {
		if err := catalog.ValidateSelf(desc); err != nil {
			return err
		}
	}
	if err := tc.AddUncommittedDescriptor(desc); err != nil {
		return err
	}
	return catalogkv.WriteDescToBatch(ctx, kvTrace, tc.settings, b, tc.codec(), desc.GetID(), desc)
}

// WriteDesc constructs a new Batch, calls WriteDescToBatch and runs it.
func (tc *Collection) WriteDesc(
	ctx context.Context, kvTrace bool, desc catalog.MutableDescriptor, txn *kv.Txn,
) error {
	b := txn.NewBatch()
	if err := tc.WriteDescToBatch(ctx, kvTrace, desc, b); err != nil {
		return err
	}
	return txn.Run(ctx, b)
}

// GetDescriptorsWithNewVersion returns all the IDVersion pairs that have
// undergone a schema change. Returns nil for no schema changes. The version
// returned for each schema change is ClusterVersion - 1, because that's the one
// that will be used when checking for table descriptor two version invariance.
func (tc *Collection) GetDescriptorsWithNewVersion() []lease.IDVersion {
	return tc.kv.getDescriptorsWithNewVersion()
}

// GetUncommittedTables returns all the tables updated or created in the
// transaction.
func (tc *Collection) GetUncommittedTables() (tables []catalog.TableDescriptor) {
	return tc.kv.getUncommittedTables()
}

// ValidateUncommittedDescriptors validates all uncommitted descriptors.
// Validation includes cross-reference checks. Referenced descriptors are
// read from the store unless they happen to also be part of the uncommitted
// descriptor set. We purposefully avoid using leased descriptors as those may
// be one version behind, in which case it's possible (and legitimate) that
// those are missing back-references which would cause validation to fail.
func (tc *Collection) ValidateUncommittedDescriptors(ctx context.Context, txn *kv.Txn) error {
	if tc.skipValidationOnWrite || !ValidateOnWriteEnabled.Get(&tc.settings.SV) {
		return nil
	}
	return tc.kv.validateUncommittedDescriptors(ctx, txn)
}

// getSyntheticOrUncommittedDescriptor attempts to look up a descriptor in the
// set of synthetic descriptors, followed by the set of uncommitted descriptors.
func (tc *Collection) getSyntheticOrUncommittedDescriptor(
	dbID descpb.ID, schemaID descpb.ID, name string, mutable bool,
) (found bool, refuseFurtherLookup bool, desc catalog.Descriptor, err error) {
	if found, sd := tc.synthetic.getByName(dbID, schemaID, name); found {
		if mutable {
			return false, false, nil, newMutableSyntheticDescriptorAssertionError(sd.GetID())
		}
		return true, false, sd, nil
	}

	var ud *uncommittedDescriptor
	refuseFurtherLookup, ud = tc.kv.getUncommittedByName(dbID, schemaID, name)
	if ud == nil {
		return false, refuseFurtherLookup, nil, nil
	}
	if mutable {
		return true, false, ud.mutable, nil
	}
	return true, false, ud.immutable, nil
}

func newMutableSyntheticDescriptorAssertionError(id descpb.ID) error {
	return errors.AssertionFailedf("attempted mutable access of synthetic descriptor %d", id)
}

// GetAllDescriptors returns all descriptors visible by the transaction,
// first checking the Collection's cached descriptors for validity if validate
// is set to true before defaulting to a key-value scan, if necessary.
func (tc *Collection) GetAllDescriptors(
	ctx context.Context, txn *kv.Txn,
) ([]catalog.Descriptor, error) {
	return tc.kv.getAllDescriptors(ctx, txn)
}

// GetAllDatabaseDescriptors returns all database descriptors visible by the
// transaction, first checking the Collection's cached descriptors for
// validity before scanning system.namespace and looking up the descriptors
// in the database cache, if necessary.
// If the argument allowMissingDesc is true, the function will return nil-s for
// missing database descriptors.
func (tc *Collection) GetAllDatabaseDescriptors(
	ctx context.Context, txn *kv.Txn,
) ([]catalog.DatabaseDescriptor, error) {
	return tc.kv.getAllDatabaseDescriptors(ctx, txn)
}

// GetSchemasForDatabase returns the schemas for a given database
// visible by the transaction. This uses the schema cache locally
// if possible, or else performs a scan on kv.
func (tc *Collection) GetSchemasForDatabase(
	ctx context.Context, txn *kv.Txn, dbID descpb.ID,
) (map[descpb.ID]string, error) {
	return tc.kv.getSchemasForDatabase(ctx, txn, dbID)
}

// GetObjectNamesAndIDs returns the names and IDs of all objects in a database and schema.
func (tc *Collection) GetObjectNamesAndIDs(
	ctx context.Context,
	txn *kv.Txn,
	dbDesc catalog.DatabaseDescriptor,
	scName string,
	flags tree.DatabaseListFlags,
) (tree.TableNames, descpb.IDs, error) {
	if ok, names, ds := tc.maybeGetVirtualObjectNamesAndIDs(
		scName, dbDesc, flags,
	); ok {
		return names, ds, nil
	}

	schemaFlags := tree.SchemaLookupFlags{
		Required:       flags.Required,
		AvoidCached:    flags.RequireMutable || flags.AvoidCached,
		IncludeDropped: flags.IncludeDropped,
		IncludeOffline: flags.IncludeOffline,
	}
	schema, err := tc.GetImmutableSchemaByName(ctx, txn, dbDesc.GetID(), scName, schemaFlags)
	if err != nil {
		return nil, nil, err
	}
	if schema == nil { // required must have been false
		return nil, nil, nil
	}

	log.Eventf(ctx, "fetching list of objects for %q", dbDesc.GetName())
	prefix := catalogkeys.MakeObjectNameKey(tc.codec(), dbDesc.GetID(), schema.GetID(), "")
	sr, err := txn.Scan(ctx, prefix, prefix.PrefixEnd(), 0)
	if err != nil {
		return nil, nil, err
	}

	alreadySeen := make(map[string]bool)
	var tableNames tree.TableNames
	var tableIDs descpb.IDs

	for _, row := range sr {
		_, tableName, err := encoding.DecodeUnsafeStringAscending(bytes.TrimPrefix(
			row.Key, prefix), nil)
		if err != nil {
			return nil, nil, err
		}
		alreadySeen[tableName] = true
		tn := tree.MakeTableNameWithSchema(tree.Name(dbDesc.GetName()), tree.Name(scName), tree.Name(tableName))
		tn.ExplicitCatalog = flags.ExplicitPrefix
		tn.ExplicitSchema = flags.ExplicitPrefix
		tableNames = append(tableNames, tn)
		tableIDs = append(tableIDs, descpb.ID(row.ValueInt()))
	}

	return tableNames, tableIDs, nil
}

func (tc *Collection) maybeGetVirtualObjectNamesAndIDs(
	scName string, dbDesc catalog.DatabaseDescriptor, flags tree.DatabaseListFlags,
) (isVirtual bool, _ tree.TableNames, _ descpb.IDs) {
	if tc.virtualSchemas == nil {
		return false, nil, nil
	}
	entry, ok := tc.virtualSchemas.GetVirtualSchema(scName)
	if !ok {
		return false, nil, nil
	}
	names := make(tree.TableNames, 0, entry.NumTables())
	IDs := make(descpb.IDs, 0, entry.NumTables())
	schemaDesc := entry.Desc()
	entry.VisitTables(func(table catalog.VirtualObject) {
		name := tree.MakeTableNameWithSchema(
			tree.Name(dbDesc.GetName()), tree.Name(schemaDesc.GetName()), tree.Name(table.Desc().GetName()))
		name.ExplicitCatalog = flags.ExplicitPrefix
		name.ExplicitSchema = flags.ExplicitPrefix
		names = append(names, name)
		IDs = append(IDs, table.Desc().GetID())
	})
	return true, names, IDs

}

// SetSyntheticDescriptors sets the provided descriptors as the synthetic
// descriptors to override all other matching descriptors during immutable
// access. An immutable copy is made if the descriptor is mutable. See the
// documentation on syntheticDescriptors.
func (tc *Collection) SetSyntheticDescriptors(descs []catalog.Descriptor) {
	tc.synthetic.set(descs)
}

func (tc *Collection) codec() keys.SQLCodec {
	return tc.leaseMgr.Codec()
}

// AddDeletedDescriptor is temporarily tracking descriptors that have been,
// deleted which from an add state without any intermediate steps
// Any descriptors marked as deleted will be skipped for the
// wait for one version logic inside descs.Txn, since they will no longer
// be inside storage.
// Note: that this happens, at time of writing, only when reverting an
// IMPORT or RESTORE.
func (tc *Collection) AddDeletedDescriptor(desc catalog.Descriptor) {
	tc.deletedDescs = append(tc.deletedDescs, desc)
}

// LeaseManager returns the lease.Manager.
func (tc *Collection) LeaseManager() *lease.Manager {
	return tc.leaseMgr
}

func (tc *Collection) maybeGetVirtualDescriptorByID(
	ctx context.Context, id descpb.ID, flags tree.CommonLookupFlags,
) (catalog.Descriptor, error) {
	if tc.virtualSchemas == nil {
		return nil, nil
	}
	if vd, found := tc.virtualSchemas.GetVirtualObjectByID(id); found {
		if flags.RequireMutable {
			vs, found := tc.virtualSchemas.GetVirtualSchemaByID(vd.Desc().GetParentSchemaID())
			if !found {
				return nil, errors.AssertionFailedf(
					"cannot resolve mutable virtual descriptor %d with unknown parent schema %d",
					id, vd.Desc().GetParentSchemaID(),
				)
			}
			return nil, catalog.NewMutableAccessToVirtualSchemaError(vs, vd.Desc().GetName())
		}
		return vd.Desc(), nil
	}
	if vs, found := tc.virtualSchemas.GetVirtualSchemaByID(id); found {
		if flags.RequireMutable {
			return nil, catalog.NewMutableAccessToVirtualSchemaError(vs, vs.Desc().GetName())
		}
	}
	return nil, nil
}
