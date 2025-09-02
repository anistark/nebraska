package api

import (
	"errors"
	"time"

	"github.com/doug-martin/goqu/v9"
	"gopkg.in/guregu/null.v4"
)

const (
	// EventUpdateComplete indicates that the update process completed. It could
	// mean a successful or failed updated, depending on the result attached to
	// the event. This applies to all events.
	EventUpdateComplete = 3

	// EventUpdateDownloadStarted indicates that the instance started
	// downloading the update package.
	EventUpdateDownloadStarted = 13

	// EventUpdateDownloadFinished indicates that the update package was
	// downloaded.
	EventUpdateDownloadFinished = 14

	// EventUpdateInstalled indicates that the update package was installed.
	EventUpdateInstalled = 800
)

const (
	// ResultFailed indicates that the operation associated with the event
	// posted failed.
	ResultFailed = 0

	// ResultSuccess indicates that the operation associated with the event
	// posted succeeded.
	ResultSuccess = 1

	// ResultSuccessReboot also indicates a successful operation, but it's
	// meant only to be used along with events of EventUpdateComplete type.
	// It's important that instances use EventUpdateComplete events in
	// combination with ResultSuccessReboot to communicate a successful update
	// completed as it has a special meaning for Nebraska in order to adjust
	// properly the rollout policies and create activity entries.
	ResultSuccessReboot = 2
)

var (
	// ErrInvalidInstance indicates that the instance provided is not valid or
	// it doesn't exist.
	ErrInvalidInstance = errors.New("nebraska: invalid instance")

	// ErrInvalidApplicationOrGroup indicates that the application or group id
	// provided are not valid or related to each other.
	ErrInvalidApplicationOrGroup = errors.New("nebraska: invalid application or group")

	// ErrInvalidEventTypeOrResult indicates that the event or result provided
	// are not valid (Nebraska only implements a subset of the Omaha protocol
	// events).
	ErrInvalidEventTypeOrResult = errors.New("nebraska: invalid event type or result")

	// ErrEventRegistrationFailed indicates that the event registration into
	// Nebraska failed.
	ErrEventRegistrationFailed = errors.New("nebraska: event registration failed")

	// ErrNoUpdateInProgress indicates that an event was received but there
	// wasn't an update in progress for the provided instance/application, so
	// it was rejected.
	ErrNoUpdateInProgress = errors.New("nebraska: no update in progress")

	// ErrFlatcarEventIgnored indicates that a Flatcar updater event was ignored.
	// This is a temporary solution to handle Flatcar specific behaviour.
	ErrFlatcarEventIgnored = errors.New("nebraska: flatcar event ignored")
)

// Event represents an event posted by an instance to Nebraska.
type Event struct {
	ID              int         `db:"id" json:"id"`
	CreatedTs       time.Time   `db:"created_ts" json:"created_ts"`
	PreviousVersion null.String `db:"previous_version" json:"previous_version"`
	ErrorCode       null.String `db:"error_code" json:"error_code"`
	InstanceID      string      `db:"instance_id" json:"instance_id"`
	ApplicationID   string      `db:"application_id" json:"application_id"`
	EventTypeID     string      `db:"event_type_id" json:"event_type_id"`
}

// RegisterEvent registers an event posted by an instance in Nebraska. The
// event will be bound to an application/group combination.
func (api *API) RegisterEvent(instanceID, appID, groupID string, etype, eresult int, previousVersion, errorCode string) error {
	var err error
	if appID, groupID, err = api.validateApplicationAndGroup(appID, groupID); err != nil {
		return err
	}
	instance, err := api.GetInstance(instanceID, appID)
	if err != nil {
		l.Info().Err(err).Msg("RegisterEvent - could not get instance, maybe it is a first contact (propagates as ErrInvalidInstance)")
		return ErrInvalidInstance
	}
	if instance.Application.ApplicationID != appID {
		return ErrInvalidApplicationOrGroup
	}
	if !instance.Application.UpdateInProgress {
		// Do not log the event when we don't know about an update going on.
		// There is no need to reset the instance state here because update_in_progress
		// is only set to "false" for states in which we will grant an update.
		return ErrNoUpdateInProgress
	}

	// Temporary hack to handle Flatcar updater specific behaviour
	if appID == flatcarAppID && etype == EventUpdateComplete && eresult == ResultSuccessReboot {
		if previousVersion == "" || previousVersion == "0.0.0.0" {
			// Do not log the Complete event for already updated instances but reset the instance state to
			// ensure it can update and is not stuck in some other state because according to the DB it,
			// e.g., is updating and thus shouldn't be granted any update. The instance can't be in a Completed
			// state because of the ErrNoUpdateInProgress check above, thus no need to cover this case here.
			// The Undefined state is chosen because the instance did not tell that it updated from a previous
			// version ("" and "0.0.0.0" are not valid but "0.0.0" is because it is used when forcing an update).
			if err := api.updateInstanceObjStatus(instance, InstanceStatusUndefined); err != nil {
				l.Error().Err(err).Msg("RegisterEvent - could not update instance status")
			}
			return ErrFlatcarEventIgnored
		}
	}

	var eventTypeID int
	query, _, err := goqu.From("event_type").
		Select("id").
		Where(goqu.C("type").Eq(etype), goqu.C("result").Eq(eresult)).
		ToSQL()
	if err != nil {
		return err
	}
	err = api.db.QueryRow(query).Scan(&eventTypeID)
	if err != nil {
		return ErrInvalidEventTypeOrResult
	}

	insertQuery, _, err := goqu.Insert("event").
		Cols("event_type_id", "instance_id", "application_id", "previous_version", "error_code").
		Vals(goqu.Vals{eventTypeID, instanceID, appID, previousVersion, errorCode}).
		ToSQL()
	if err != nil {
		return err
	}
	_, err = api.db.Exec(insertQuery)

	if err != nil {
		return ErrEventRegistrationFailed
	}

	lastUpdateVersion := instance.Application.LastUpdateVersion.String
	if err := api.triggerEventConsequences(instanceID, appID, groupID, lastUpdateVersion, etype, eresult); err != nil {
		l.Error().Err(err).Msgf("RegisterEvent - could not trigger event consequences")
	}

	return nil
}

// triggerEventConsequences is in charge of triggering the consequences of a
// given event. Depending on the type of the event and its result, the status
// of the instance may be updated, new activity entries could be created, etc.
func (api *API) triggerEventConsequences(instanceID, appID, groupID, lastUpdateVersion string, etype, result int) error {
	group, err := api.GetGroup(groupID)
	if err != nil {
		return err
	}

	// We allow the plain ResultSuccess here only if the app is not Flatcar because Flatcar is relying on
	// having only the update-complete logic on ResultSuccessReboot.
	if etype == EventUpdateComplete && (result == ResultSuccessReboot || (appID != flatcarAppID && result == ResultSuccess)) {
		if err := api.updateInstanceStatus(instanceID, appID, InstanceStatusComplete); err != nil {
			l.Error().Err(err).Msg("triggerEventConsequences - could not update instance status")
		}

		updatesStats, err := api.getGroupUpdatesStats(group)
		if err != nil {
			return err
		}
		if updatesStats.UpdatesToCurrentVersionSucceeded == updatesStats.TotalInstances {
			if err := api.setGroupRolloutInProgress(groupID, false); err != nil {
				l.Error().Err(err).Msg("triggerEventConsequences - could not set rollout progress")
			}
			if err := api.newGroupActivityEntry(activityRolloutFinished, activitySuccess, lastUpdateVersion, appID, groupID); err != nil {
				l.Error().Err(err).Msg("triggerEventConsequences - could not add group activity")
			}
		}
	}

	if etype == EventUpdateDownloadStarted && result == ResultSuccess {
		if err := api.updateInstanceStatus(instanceID, appID, InstanceStatusDownloading); err != nil {
			l.Error().Err(err).Msg("triggerEventConsequences - could not update instance status")
		}
	}

	if etype == EventUpdateDownloadFinished && result == ResultSuccess {
		if err := api.updateInstanceStatus(instanceID, appID, InstanceStatusDownloaded); err != nil {
			l.Error().Err(err).Msg("triggerEventConsequences - could not update instance status")
		}
	}

	if etype == EventUpdateInstalled && result == ResultSuccess {
		if err := api.updateInstanceStatus(instanceID, appID, InstanceStatusInstalled); err != nil {
			l.Error().Err(err).Msg("triggerEventConsequences - could not update instance status")
		}
	}

	if result == ResultFailed {
		if err := api.updateInstanceStatus(instanceID, appID, InstanceStatusError); err != nil {
			l.Error().Err(err).Msg("triggerEventConsequences - could not update instance status")
		}
		if err := api.newInstanceActivityEntry(activityInstanceUpdateFailed, activityError, lastUpdateVersion, appID, groupID, instanceID); err != nil {
			l.Error().Err(err).Msg("triggerEventConsequences - could not add instance activity")
		}

		if api.disableUpdatesOnFailedRollout {
			updatesStats, err := api.getGroupUpdatesStats(group)
			if err != nil {
				return err
			}
			if updatesStats.UpdatesToCurrentVersionAttempted == 1 {
				if err := api.disableUpdates(groupID); err != nil {
					l.Error().Err(err).Msg("triggerEventConsequences - could not disable updates")
				}
				if err := api.setGroupRolloutInProgress(groupID, false); err != nil {
					l.Error().Err(err).Msg("triggerEventConsequences - could not set rollout progress")
				}
				if err := api.newGroupActivityEntry(activityRolloutFailed, activityError, lastUpdateVersion, appID, groupID); err != nil {
					l.Error().Err(err).Msg("triggerEventConsequences - could not add group activity")
				}
			}
		}
	}

	return nil
}

func (api *API) GetEvent(instanceID string, appID string, timestamp time.Time) (null.String, error) {
	query, _, err := goqu.From("event").
		Select("error_code").
		Where(goqu.C("instance_id").Eq(instanceID)).
		Where(goqu.C("application_id").Eq(appID)).
		Where(goqu.C("created_ts").Lte(timestamp)).
		Order(goqu.C("created_ts").Desc()).
		Limit(1).
		ToSQL()
	if err != nil {
		return null.NewString("", true), err
	}
	var errCode null.String
	err = api.db.QueryRow(query).Scan(&errCode)
	if err != nil {
		return null.NewString("", true), err
	}
	return errCode, nil
}
