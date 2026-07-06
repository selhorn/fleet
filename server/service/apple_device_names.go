package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/fleetdm/fleet/v4/server/contexts/ctxerr"
	"github.com/fleetdm/fleet/v4/server/fleet"
	apple_mdm "github.com/fleetdm/fleet/v4/server/mdm/apple"
	"github.com/google/uuid"
)

// reconcileHostDeviceNamesBatchSize bounds how many queued rename rows a
// single cron tick processes; remaining rows are picked up on subsequent
// ticks, amortizing command delivery for large teams.
//
// var (not const) so tests can override it.
var reconcileHostDeviceNamesBatchSize = 500

// deviceNameMaxBytes is Apple's limit on a device name; names resolving
// longer than this are not sent and the enforcement row is marked failed.
const deviceNameMaxBytes = 63

// ReconcileHostDeviceNames runs one pass of host-name template enforcement:
// for each host whose enforcement row is queued (status NULL), it resolves
// the host's team name template and either enqueues a Settings/DeviceName
// command or records the outcome directly (name already matching → verified;
// resolved name unusable → failed).
func ReconcileHostDeviceNames(
	ctx context.Context,
	ds fleet.Datastore,
	commander *apple_mdm.MDMAppleCommander,
	logger *slog.Logger,
) error {
	appConfig, err := ds.AppConfig(ctx)
	if err != nil {
		return ctxerr.Wrap(ctx, err, "reading app config")
	}
	if !appConfig.MDM.EnabledAndConfigured {
		return nil
	}

	pending, err := ds.ListHostsPendingDeviceNameCommand(ctx, reconcileHostDeviceNamesBatchSize)
	if err != nil {
		return ctxerr.Wrap(ctx, err, "list hosts pending device name command")
	}
	if len(pending) == 0 {
		return nil
	}

	templates := make(map[uint]string) // team ID → name template
	for _, host := range pending {
		if host.TeamID == nil {
			// Enforcement rows are only created for hosts on a team with a
			// template; the host moved to "no team" between cron runs and its
			// row is reconciled by the transfer path. Leave it queued.
			continue
		}
		tmpl, ok := templates[*host.TeamID]
		if !ok {
			mdmConfig, err := ds.TeamMDMConfig(ctx, *host.TeamID)
			if err != nil {
				if fleet.IsNotFound(err) {
					// team deleted between cron runs
					templates[*host.TeamID] = ""
					continue
				}
				return ctxerr.Wrap(ctx, err, "get team mdm config for device name")
			}
			tmpl = mdmConfig.HostNameTemplate
			templates[*host.TeamID] = tmpl
		}
		if tmpl == "" {
			// Template cleared between cron runs; the clear path deletes the
			// rows, so nothing to enforce here.
			continue
		}

		resolved := fleet.ResolveHostNameTemplate(tmpl, &fleet.Host{
			UUID:           host.HostUUID,
			HardwareSerial: host.HardwareSerial,
			Platform:       host.Platform,
		})
		switch {
		case len(resolved) > deviceNameMaxBytes:
			// The resolved name is not stored: it can exceed the column width,
			// and failed rows are never compared against reported names.
			if err := ds.SetHostDeviceNameResolveResult(ctx, host.HostUUID, fleet.MDMDeliveryFailed, "",
				"Resolved name exceeds 63 bytes."); err != nil {
				return ctxerr.Wrap(ctx, err, "mark device name row failed for too-long name")
			}
			logger.InfoContext(ctx, "host name template resolves past the device name limit, not sending command",
				"host_uuid", host.HostUUID, "resolved_bytes", len(resolved))
		case resolved == host.ComputerName:
			// The device already carries the resolved name; no command needed.
			if err := ds.SetHostDeviceNameResolveResult(ctx, host.HostUUID, fleet.MDMDeliveryVerified, resolved, ""); err != nil {
				return ctxerr.Wrap(ctx, err, "mark device name row verified for matching name")
			}
		default:
			cmdUUID := fleet.DeviceNameCommandUUIDPrefix + uuid.NewString()
			if err := commander.DeviceNameSetting(ctx, host.HostUUID, cmdUUID, resolved); err != nil {
				var apnsErr *apple_mdm.APNSDeliveryError
				if !errors.As(err, &apnsErr) {
					// The command was not persisted; leave the row queued so the
					// next cron run retries this host, and move on so one bad
					// host doesn't starve the rest of the batch.
					logger.ErrorContext(ctx, "enqueue device name command", "host_uuid", host.HostUUID, "err", err)
					continue
				}
				// The command was persisted to the MDM queue but the APNs push
				// failed: the device will still receive it on its next check-in,
				// so record the command as sent — resetting or retrying here
				// would enqueue a duplicate.
				logger.WarnContext(ctx, "device name command enqueued but APNs push failed",
					"host_uuid", host.HostUUID, "command_uuid", cmdUUID, "err", err)
			}
			if err := ds.SetHostDeviceNameCommandSent(ctx, host.HostUUID, cmdUUID, resolved); err != nil {
				// The command was sent but recording it failed; log and move on
				// rather than aborting the batch, consistent with the
				// enqueue-failure handling above. The row stays queued and a
				// later cron run re-sends; the device resolves to the latest
				// command, and the superseded one's result is dropped as stale.
				logger.ErrorContext(ctx, "mark device name command sent", "host_uuid", host.HostUUID, "command_uuid", cmdUUID, "err", err)
				continue
			}
		}
	}
	return nil
}
