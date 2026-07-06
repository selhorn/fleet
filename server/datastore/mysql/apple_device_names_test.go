package mysql

import (
	"testing"
	"time"

	"github.com/fleetdm/fleet/v4/server/fleet"
	apple_mdm "github.com/fleetdm/fleet/v4/server/mdm/apple"
	"github.com/fleetdm/fleet/v4/server/test"
	"github.com/stretchr/testify/require"
)

func TestHostDeviceNames(t *testing.T) {
	ds := CreateMySQLDS(t)

	cases := []struct {
		name string
		fn   func(t *testing.T, ds *Datastore)
	}{
		{"Eligibility", testHostDeviceNamesEligibility},
		{"CommandLifecycle", testHostDeviceNamesCommandLifecycle},
		{"Verify", testHostDeviceNamesVerify},
		{"Resend", testHostDeviceNamesResend},
		{"Reconcile", testHostDeviceNamesReconcile},
		{"HostDeletionCleanup", testHostDeviceNamesHostDeletionCleanup},
		{"FullLifecycle", testHostDeviceNamesFullLifecycle},
		{"ResolveResult", testHostDeviceNamesResolveResult},
		{"VerifyGracePeriod", testHostDeviceNamesVerifyGracePeriod},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer TruncateTables(t, ds)
			c.fn(t, ds)
		})
	}
}

// enrollAppleHostForDeviceName creates an Apple host and enrolls it in Fleet's
// MDM. When personal is true, the host is marked as a personal (BYOD) enrollment.
func enrollAppleHostForDeviceName(t *testing.T, ds *Datastore, name, platform string, teamID uint, personal bool) *fleet.Host {
	ctx := t.Context()
	host := test.NewHost(t, ds, name, "1.1.1.1", name+"-key", name+"-uuid", time.Now(),
		test.WithPlatform(platform), test.WithTeamID(teamID))

	ac, err := ds.AppConfig(ctx)
	require.NoError(t, err)
	serverURL, err := apple_mdm.ResolveAppleEnrollMDMURL(ac.ServerSettings.ServerURL)
	require.NoError(t, err)

	nanoEnroll(t, ds, host, false)
	require.NoError(t, ds.SetOrUpdateMDMData(ctx, host.ID, false, true, serverURL, true, fleet.WellKnownMDMFleet, "", personal))
	return host
}

func getDeviceNameRow(t *testing.T, ds *Datastore, hostUUID string) *fleet.HostDeviceNameEnforcement {
	enforcement, err := ds.GetHostDeviceNameEnforcement(t.Context(), hostUUID)
	require.NoError(t, err)
	return enforcement
}

func testHostDeviceNamesEligibility(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	team, err := ds.NewTeam(ctx, &fleet.Team{Name: "eligibility-team"})
	require.NoError(t, err)

	macHost := enrollAppleHostForDeviceName(t, ds, "mac", "darwin", team.ID, false)
	iosHost := enrollAppleHostForDeviceName(t, ds, "ios", "ios", team.ID, false)
	ipadHost := enrollAppleHostForDeviceName(t, ds, "ipad", "ipados", team.ID, false)
	byodHost := enrollAppleHostForDeviceName(t, ds, "byod", "ios", team.ID, true)
	winHost := enrollAppleHostForDeviceName(t, ds, "win", "windows", team.ID, false)

	// linux and non-enrolled darwin hosts are never eligible.
	linuxHost := test.NewHost(t, ds, "linux", "1.1.1.2", "linux-key", "linux-uuid", time.Now(),
		test.WithPlatform("linux"), test.WithTeamID(team.ID))
	notEnrolled := test.NewHost(t, ds, "not-enrolled", "1.1.1.3", "ne-key", "ne-uuid", time.Now(),
		test.WithPlatform("darwin"), test.WithTeamID(team.ID))

	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))

	// Only Apple, Fleet-MDM enrolled, non-personal hosts get a row.
	eligible := []*fleet.Host{macHost, iosHost, ipadHost}
	for _, h := range eligible {
		row := getDeviceNameRow(t, ds, h.UUID)
		require.Nil(t, row.Status, "eligible host %s should be queued (NULL status)", h.Hostname)
	}

	ineligible := []*fleet.Host{byodHost, winHost, linuxHost, notEnrolled}
	for _, h := range ineligible {
		_, err := ds.GetHostDeviceNameEnforcement(ctx, h.UUID)
		require.True(t, fleet.IsNotFound(err), "ineligible host %s should have no row", h.Hostname)
	}

	// A re-save re-queues even hosts that had already been verified: mark one
	// verified, then bulk upsert resets its status back to NULL (ON DUPLICATE KEY
	// UPDATE branch).
	_, err = ds.writer(ctx).ExecContext(ctx, `UPDATE host_mdm_apple_device_names SET status = ? WHERE host_uuid = ?`, fleet.MDMDeliveryVerified, macHost.UUID)
	require.NoError(t, err)
	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))
	require.Nil(t, getDeviceNameRow(t, ds, macHost.UUID).Status, "re-save should reset a verified host back to queued")

	// A second eligible host in another team, to prove delete is team-scoped.
	otherTeam, err := ds.NewTeam(ctx, &fleet.Team{Name: "eligibility-other-team"})
	require.NoError(t, err)
	otherHost := enrollAppleHostForDeviceName(t, ds, "other-mac", "darwin", otherTeam.ID, false)
	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, otherTeam.ID))

	// Clearing the team removes every row for that team and leaves other teams' rows.
	require.NoError(t, ds.DeleteHostDeviceNameEnforcementForTeam(ctx, team.ID))
	for _, h := range eligible {
		_, err := ds.GetHostDeviceNameEnforcement(ctx, h.UUID)
		require.True(t, fleet.IsNotFound(err), "row for %s should be deleted", h.Hostname)
	}
	require.Nil(t, getDeviceNameRow(t, ds, otherHost.UUID).Status, "other team's row must survive the delete")
}

func testHostDeviceNamesCommandLifecycle(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	team, err := ds.NewTeam(ctx, &fleet.Team{Name: "lifecycle-team"})
	require.NoError(t, err)

	host := enrollAppleHostForDeviceName(t, ds, "mac", "darwin", team.ID, false)
	// Give the host a serial so we can assert it flows through ListHostsPending.
	_, err = ds.writer(ctx).ExecContext(ctx, `UPDATE hosts SET hardware_serial = ?, computer_name = ? WHERE id = ?`, "SERIAL123", "old-name", host.ID)
	require.NoError(t, err)

	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))

	// The queued host shows up in the pending list with its host details.
	pending, err := ds.ListHostsPendingDeviceNameCommand(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, host.ID, pending[0].HostID)
	require.Equal(t, host.UUID, pending[0].HostUUID)
	require.Equal(t, "SERIAL123", pending[0].HardwareSerial)
	require.Equal(t, "darwin", pending[0].Platform)
	require.Equal(t, "old-name", pending[0].ComputerName)
	require.NotNil(t, pending[0].TeamID)
	require.Equal(t, team.ID, *pending[0].TeamID)

	// Marking the command as sent moves the row to pending and records details.
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-cmd-1", "WS-SERIAL123"))
	row := getDeviceNameRow(t, ds, host.UUID)
	require.NotNil(t, row.Status)
	require.Equal(t, fleet.MDMDeliveryPending, *row.Status)
	require.NotNil(t, row.CommandUUID)
	require.Equal(t, "DEVNAME-cmd-1", *row.CommandUUID)
	require.NotNil(t, row.ExpectedDeviceName)
	require.Equal(t, "WS-SERIAL123", *row.ExpectedDeviceName)

	// It is no longer pending.
	pending, err = ds.ListHostsPendingDeviceNameCommand(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending)

	// An acknowledgment moves the row to verifying and renames the host in Fleet
	// (computer_name, hostname, display name) to the expected name, atomically.
	require.NoError(t, ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-cmd-1", fleet.MDMDeliveryVerifying, ""))
	require.Equal(t, fleet.MDMDeliveryVerifying, *getDeviceNameRow(t, ds, host.UUID).Status)
	renamed, err := ds.Host(ctx, host.ID)
	require.NoError(t, err)
	require.Equal(t, "WS-SERIAL123", renamed.ComputerName)
	require.Equal(t, "WS-SERIAL123", renamed.Hostname)
	require.Equal(t, "WS-SERIAL123", renamed.DisplayName())

	// An error result records the Apple detail and does not rename the host.
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-cmd-2", "WS-SERIAL123"))
	require.NoError(t, ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-cmd-2", fleet.MDMDeliveryFailed, "Apple error chain"))
	row = getDeviceNameRow(t, ds, host.UUID)
	require.Equal(t, fleet.MDMDeliveryFailed, *row.Status)
	require.Equal(t, "Apple error chain", row.Detail)

	// An unknown command UUID is a not-found error.
	err = ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-nope", fleet.MDMDeliveryVerifying, "")
	require.True(t, fleet.IsNotFound(err))

	// Getting an enforcement row for a host with none is a not-found error.
	_, err = ds.GetHostDeviceNameEnforcement(ctx, "missing-uuid")
	require.True(t, fleet.IsNotFound(err))
}

func testHostDeviceNamesVerify(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	team, err := ds.NewTeam(ctx, &fleet.Team{Name: "verify-team"})
	require.NoError(t, err)
	host := enrollAppleHostForDeviceName(t, ds, "mac", "darwin", team.ID, false)

	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-cmd", "WS-1"))

	// A NULL/pending row is left untouched by verification.
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "WS-1"))
	require.Equal(t, fleet.MDMDeliveryPending, *getDeviceNameRow(t, ds, host.UUID).Status)

	// Move to verifying, then a matching report verifies it.
	err = ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-cmd", fleet.MDMDeliveryVerifying, "")
	require.NoError(t, err)
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "WS-1"))
	require.Equal(t, fleet.MDMDeliveryVerified, *getDeviceNameRow(t, ds, host.UUID).Status)

	// Re-verifying an already-verified, still-matching row is a no-op: status
	// stays verified and the row is not rewritten (updated_at unchanged).
	verifiedAt := getDeviceNameRow(t, ds, host.UUID).UpdatedAt
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "WS-1"))
	afterReverify := getDeviceNameRow(t, ds, host.UUID)
	require.Equal(t, fleet.MDMDeliveryVerified, *afterReverify.Status)
	require.True(t, afterReverify.UpdatedAt.Equal(verifiedAt), "re-verifying a matching row must not rewrite it")

	// A later mismatching report is drift: verified -> failed with a detail.
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "renamed-by-user"))
	row := getDeviceNameRow(t, ds, host.UUID)
	require.Equal(t, fleet.MDMDeliveryFailed, *row.Status)
	require.NotEmpty(t, row.Detail)

	// A failed row is left untouched even by a later matching report: only
	// verifying/verified rows are reconciled, so recovery requires an explicit
	// resend rather than silent self-healing.
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "WS-1"))
	require.Equal(t, fleet.MDMDeliveryFailed, *getDeviceNameRow(t, ds, host.UUID).Status)

	// A host with no row is a no-op (no error).
	require.NoError(t, ds.VerifyHostDeviceName(ctx, "missing-uuid", "anything"))
}

func testHostDeviceNamesResend(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	team, err := ds.NewTeam(ctx, &fleet.Team{Name: "resend-team"})
	require.NoError(t, err)
	host := enrollAppleHostForDeviceName(t, ds, "mac", "darwin", team.ID, false)

	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-cmd", "WS-1"))
	err = ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-cmd", fleet.MDMDeliveryFailed, "boom")
	require.NoError(t, err)
	require.Equal(t, fleet.MDMDeliveryFailed, *getDeviceNameRow(t, ds, host.UUID).Status)

	// Resend resets the status to NULL so the cron re-enqueues it, and clears the
	// previous command UUID so a late ack for it can't match this row.
	require.NoError(t, ds.ResendHostDeviceName(ctx, host.UUID))
	row := getDeviceNameRow(t, ds, host.UUID)
	require.Nil(t, row.Status)
	require.Nil(t, row.CommandUUID)

	// The previous command's late acknowledgment no longer matches any row.
	err = ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-cmd", fleet.MDMDeliveryVerifying, "")
	require.True(t, fleet.IsNotFound(err))
	require.Nil(t, getDeviceNameRow(t, ds, host.UUID).Status, "late ack must not resurrect the row")

	pending, err := ds.ListHostsPendingDeviceNameCommand(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, host.UUID, pending[0].HostUUID)
}

func testHostDeviceNamesReconcile(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	// setTemplate writes name_template into the team's config JSON directly so
	// this test does not depend on the TeamMDM struct field (sub-issue #48621).
	setTemplate := func(teamID uint, tmpl string) {
		_, err := ds.writer(ctx).ExecContext(ctx,
			`UPDATE teams SET config = JSON_SET(config, '$.mdm.name_template', ?) WHERE id = ?`, tmpl, teamID)
		require.NoError(t, err)
	}

	withTemplate, err := ds.NewTeam(ctx, &fleet.Team{Name: "with-template"})
	require.NoError(t, err)
	setTemplate(withTemplate.ID, "WS-$FLEET_VAR_HOST_HARDWARE_SERIAL")

	noTemplate, err := ds.NewTeam(ctx, &fleet.Team{Name: "no-template"})
	require.NoError(t, err)

	hostWith := enrollAppleHostForDeviceName(t, ds, "with", "darwin", withTemplate.ID, false)
	hostWithout := enrollAppleHostForDeviceName(t, ds, "without", "darwin", noTemplate.ID, false)
	hostByod := enrollAppleHostForDeviceName(t, ds, "byod", "ios", withTemplate.ID, true)

	// Reconcile upserts rows for eligible hosts whose team has a template, and
	// leaves template-less / ineligible hosts without a row.
	require.NoError(t, ds.ReconcileHostDeviceNamesForHosts(ctx, []uint{hostWith.ID, hostWithout.ID, hostByod.ID}))
	require.Nil(t, getDeviceNameRow(t, ds, hostWith.UUID).Status)
	for _, h := range []*fleet.Host{hostWithout, hostByod} {
		_, err := ds.GetHostDeviceNameEnforcement(ctx, h.UUID)
		require.True(t, fleet.IsNotFound(err), "host %s should have no row", h.Hostname)
	}

	// Simulate a transfer: the host moves to the template-less team. Reconcile
	// must delete its now-orphaned row.
	_, err = ds.writer(ctx).ExecContext(ctx, `UPDATE hosts SET team_id = ? WHERE id = ?`, noTemplate.ID, hostWith.ID)
	require.NoError(t, err)
	require.NoError(t, ds.ReconcileHostDeviceNamesForHosts(ctx, []uint{hostWith.ID}))
	_, err = ds.GetHostDeviceNameEnforcement(ctx, hostWith.UUID)
	require.True(t, fleet.IsNotFound(err), "row should be deleted after transfer to template-less team")

	// Transfer back to the template team: reconcile re-creates the queued row.
	_, err = ds.writer(ctx).ExecContext(ctx, `UPDATE hosts SET team_id = ? WHERE id = ?`, withTemplate.ID, hostWith.ID)
	require.NoError(t, err)
	require.NoError(t, ds.ReconcileHostDeviceNamesForHosts(ctx, []uint{hostWith.ID}))
	require.Nil(t, getDeviceNameRow(t, ds, hostWith.UUID).Status)

	// An empty host list is a no-op.
	require.NoError(t, ds.ReconcileHostDeviceNamesForHosts(ctx, nil))
}

func testHostDeviceNamesHostDeletionCleanup(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	team, err := ds.NewTeam(ctx, &fleet.Team{Name: "deletion-team"})
	require.NoError(t, err)
	host := enrollAppleHostForDeviceName(t, ds, "mac", "darwin", team.ID, false)

	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))
	require.Nil(t, getDeviceNameRow(t, ds, host.UUID).Status)

	// Deleting the host must remove its enforcement row (no FK cascades it).
	require.NoError(t, ds.DeleteHost(ctx, host.ID))
	_, err = ds.GetHostDeviceNameEnforcement(ctx, host.UUID)
	require.True(t, fleet.IsNotFound(err), "enforcement row must be deleted with the host")
}

func testHostDeviceNamesResolveResult(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	team, err := ds.NewTeam(ctx, &fleet.Team{Name: "resolve-team"})
	require.NoError(t, err)
	host := enrollAppleHostForDeviceName(t, ds, "mac", "darwin", team.ID, false)

	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))

	// A too-long resolution fails the row without sending a command; the row
	// leaves the pending list so the cron does not retry it.
	require.NoError(t, ds.SetHostDeviceNameResolveResult(ctx, host.UUID, fleet.MDMDeliveryFailed, "", "Resolved name exceeds 63 bytes."))
	row := getDeviceNameRow(t, ds, host.UUID)
	require.NotNil(t, row.Status)
	require.Equal(t, fleet.MDMDeliveryFailed, *row.Status)
	require.Equal(t, "Resolved name exceeds 63 bytes.", row.Detail)
	require.Nil(t, row.CommandUUID)
	pending, err := ds.ListHostsPendingDeviceNameCommand(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending)

	// An already-matching host goes straight to verified with the resolved name
	// recorded, so later reports can still detect drift from that name.
	require.NoError(t, ds.ResendHostDeviceName(ctx, host.UUID))
	require.NoError(t, ds.SetHostDeviceNameResolveResult(ctx, host.UUID, fleet.MDMDeliveryVerified, "WS-1", ""))
	row = getDeviceNameRow(t, ds, host.UUID)
	require.Equal(t, fleet.MDMDeliveryVerified, *row.Status)
	require.NotNil(t, row.ExpectedDeviceName)
	require.Equal(t, "WS-1", *row.ExpectedDeviceName)
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "renamed-on-device"))
	require.Equal(t, fleet.MDMDeliveryFailed, *getDeviceNameRow(t, ds, host.UUID).Status)

	// Recording a resolve result clears any previously sent command UUID, so a
	// stale result for that command can't overwrite the outcome.
	require.NoError(t, ds.ResendHostDeviceName(ctx, host.UUID))
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-stale", "WS-1"))
	require.NoError(t, ds.SetHostDeviceNameResolveResult(ctx, host.UUID, fleet.MDMDeliveryVerified, "WS-1", ""))
	err = ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-stale", fleet.MDMDeliveryFailed, "boom")
	require.True(t, fleet.IsNotFound(err))
	require.Equal(t, fleet.MDMDeliveryVerified, *getDeviceNameRow(t, ds, host.UUID).Status)

	// A host with no row is a no-op (no error).
	require.NoError(t, ds.SetHostDeviceNameResolveResult(ctx, "missing-uuid", fleet.MDMDeliveryVerified, "WS-1", ""))
}

func testHostDeviceNamesVerifyGracePeriod(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	team, err := ds.NewTeam(ctx, &fleet.Team{Name: "grace-team"})
	require.NoError(t, err)
	host := enrollAppleHostForDeviceName(t, ds, "mac", "darwin", team.ID, false)

	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-cmd", "WS-1"))
	err = ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-cmd", fleet.MDMDeliveryVerifying, "")
	require.NoError(t, err)

	// A mismatching report arriving shortly after the acknowledgment is a report
	// that was generated before the device applied the rename, not drift: the row
	// stays verifying and waits for a fresh report.
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "stale-pre-rename-name"))
	require.Equal(t, fleet.MDMDeliveryVerifying, *getDeviceNameRow(t, ds, host.UUID).Status)

	// A matching report is trusted at any time.
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "WS-1"))
	require.Equal(t, fleet.MDMDeliveryVerified, *getDeviceNameRow(t, ds, host.UUID).Status)

	// A mismatch on a verified row is genuine drift, grace period or not: the
	// verified state was reached by a fresh post-rename report, so a later
	// mismatch means the device was renamed off-template.
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "renamed-by-user"))
	require.Equal(t, fleet.MDMDeliveryFailed, *getDeviceNameRow(t, ds, host.UUID).Status)

	// Once the grace period has elapsed, a mismatch on a still-verifying row is
	// no longer explainable as an in-flight stale report and fails the row.
	require.NoError(t, ds.ResendHostDeviceName(ctx, host.UUID))
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-cmd-2", "WS-1"))
	err = ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-cmd-2", fleet.MDMDeliveryVerifying, "")
	require.NoError(t, err)
	_, err = ds.writer(ctx).ExecContext(ctx,
		`UPDATE host_mdm_apple_device_names SET updated_at = DATE_SUB(NOW(6), INTERVAL 1 HOUR) WHERE host_uuid = ?`, host.UUID)
	require.NoError(t, err)
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "still-the-old-name"))
	row := getDeviceNameRow(t, ds, host.UUID)
	require.Equal(t, fleet.MDMDeliveryFailed, *row.Status)
	require.NotEmpty(t, row.Detail)
}

// testHostDeviceNamesFullLifecycle walks a single host through the entire
// enforcement state machine in the order the real actors drive it: admin saves a
// template (bulk upsert), the cron picks up the queued row and sends a command,
// the MDM result handler acks it, name ingestion verifies it, the device drifts,
// the admin resends, and finally a second command supersedes an in-flight one so
// the stale ack is dropped.
func testHostDeviceNamesFullLifecycle(t *testing.T, ds *Datastore) {
	ctx := t.Context()

	team, err := ds.NewTeam(ctx, &fleet.Team{Name: "lifecycle-team"})
	require.NoError(t, err)
	host := enrollAppleHostForDeviceName(t, ds, "mac", "darwin", team.ID, false)
	_, err = ds.writer(ctx).ExecContext(ctx, `UPDATE hosts SET hardware_serial = ?, computer_name = ? WHERE id = ?`, "SERIAL1", "old-name", host.ID)
	require.NoError(t, err)

	// 1. Admin saves a template -> the host is queued (status NULL).
	require.NoError(t, ds.BulkUpsertHostDeviceNameEnforcement(ctx, team.ID))
	require.Nil(t, getDeviceNameRow(t, ds, host.UUID).Status)

	// 2. Cron picks up the queued row and enqueues a command.
	pending, err := ds.ListHostsPendingDeviceNameCommand(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, host.UUID, pending[0].HostUUID)
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-1", "WS-SERIAL1"))
	require.Equal(t, fleet.MDMDeliveryPending, *getDeviceNameRow(t, ds, host.UUID).Status)

	// 3. MDM acks the command -> row goes verifying and the host is renamed in Fleet.
	require.NoError(t, ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-1", fleet.MDMDeliveryVerifying, ""))
	require.Equal(t, fleet.MDMDeliveryVerifying, *getDeviceNameRow(t, ds, host.UUID).Status)

	// 4. Name ingestion reports the matching name -> verified.
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "WS-SERIAL1"))
	require.Equal(t, fleet.MDMDeliveryVerified, *getDeviceNameRow(t, ds, host.UUID).Status)

	// 5. The device drifts (renamed on-device) -> failed with a detail.
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "renamed-on-device"))
	failed := getDeviceNameRow(t, ds, host.UUID)
	require.Equal(t, fleet.MDMDeliveryFailed, *failed.Status)
	require.NotEmpty(t, failed.Detail)

	// 6. Admin clicks Resend -> back to queued, and the cron sees it again.
	require.NoError(t, ds.ResendHostDeviceName(ctx, host.UUID))
	require.Nil(t, getDeviceNameRow(t, ds, host.UUID).Status)
	pending, err = ds.ListHostsPendingDeviceNameCommand(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// 7. A second command supersedes an in-flight one: the cron sends command 2
	// while command 1 is still outstanding, then command 1's stale ack arrives.
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-2a", "WS-SERIAL1"))
	require.NoError(t, ds.SetHostDeviceNameCommandSent(ctx, host.UUID, "DEVNAME-2b", "WS-SERIAL1"))

	// The superseded command's ack no longer matches the row -> not found, and
	// the row is untouched (still pending on the newest command).
	err = ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-2a", fleet.MDMDeliveryVerifying, "")
	require.True(t, fleet.IsNotFound(err))
	require.Equal(t, fleet.MDMDeliveryPending, *getDeviceNameRow(t, ds, host.UUID).Status)

	// The newest command's ack is applied and renames the host.
	require.NoError(t, ds.UpdateHostDeviceNameStatusFromCommand(ctx, "DEVNAME-2b", fleet.MDMDeliveryVerifying, ""))
	require.NoError(t, ds.VerifyHostDeviceName(ctx, host.UUID, "WS-SERIAL1"))
	require.Equal(t, fleet.MDMDeliveryVerified, *getDeviceNameRow(t, ds, host.UUID).Status)
}
