package api_test

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flatcar/nebraska/backend/pkg/api"
	"github.com/flatcar/nebraska/backend/pkg/codegen"
)

func TestListActivity(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		// establish DB connection
		db := newDBForTest(t)
		teamID := getTeamID(t, db)

		endTime := time.Now()
		startTime := time.Now().Add(time.Duration(-1 * 24 * 7 * time.Hour))
		activitiesDB, err := db.GetActivity(teamID, api.ActivityQueryParams{Start: startTime, End: endTime})
		require.NoError(t, err)
		require.NotNil(t, activitiesDB)

		// fetch activity from api
		url := fmt.Sprintf("%s/api/activity?start=%s&end=%s", os.Getenv("NEBRASKA_TEST_SERVER_URL"), startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))
		method := "GET"

		// response
		var activityResp codegen.ActivityPage

		httpDo(t, url, method, nil, http.StatusOK, "json", &activityResp)

		activities := activityResp.Activities

		assert.Equal(t, len(activitiesDB), len(activities))
		for i := range activitiesDB {
			assert.Equal(t, activitiesDB[i].AppID.String, activities[i].AppID)
			assert.Equal(t, activitiesDB[i].GroupID.String, activities[i].GroupID)
			assert.Equal(t, activitiesDB[i].GroupName.String, activities[i].GroupName)
		}
	})
}
