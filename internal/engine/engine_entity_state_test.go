package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// writeEntityForStateTest writes one engram with an inline entity and returns the entity name.
func writeEntityForStateTest(t *testing.T, eng *Engine, vault, name, entityType string) {
	t.Helper()
	_, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault:   vault,
		Content: name + " is a thing",
		Concept: "test entity",
		Entities: []mbp.InlineEntity{
			{Name: name, Type: entityType},
		},
	})
	require.NoError(t, err)
}

func TestSetEntityState_PreservesTypeWhenNotProvided(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityForStateTest(t, eng, "default", "Modbus", "protocol")

	// Change state only — entityType param is empty.
	err := eng.SetEntityState(ctx, "Modbus", "deprecated", "", "")
	require.NoError(t, err)

	rec, err := eng.store.GetEntityRecord(ctx, "modbus")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "deprecated", rec.State, "state should be updated")
	require.Equal(t, "protocol", rec.Type, "type should be preserved when not provided")
}

func TestSetEntityState_UpdatesTypeWhenProvided(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Entity created with wrong type from early enrichment.
	writeEntityForStateTest(t, eng, "default", "2014/53/EU", "other")

	// Correct the type while keeping state active.
	err := eng.SetEntityState(ctx, "2014/53/EU", "active", "", "directive")
	require.NoError(t, err)

	rec, err := eng.store.GetEntityRecord(ctx, "2014/53/eu")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "active", rec.State)
	require.Equal(t, "directive", rec.Type, "type should be updated to the provided value")
}

func TestSetEntityState_UpdatesBothStateAndType(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityForStateTest(t, eng, "default", "Bridge Dongle", "other")

	err := eng.SetEntityState(ctx, "Bridge Dongle", "deprecated", "", "module")
	require.NoError(t, err)

	rec, err := eng.store.GetEntityRecord(ctx, "bridge dongle")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "deprecated", rec.State)
	require.Equal(t, "module", rec.Type)
}

func TestSetEntityState_NotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := eng.SetEntityState(ctx, "ghost entity", "deprecated", "", "")
	require.Error(t, err, "should error for entity that does not exist")
}
