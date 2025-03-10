package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/lxc/lxd/lxd/util"
)

// SnapshotScheduleAliases contains the mapping of scheduling aliases to cron syntax
// including placeholders for scheduled time obfuscation.
var SnapshotScheduleAliases = map[string]string{
	"@hourly":   "%s * * * *",
	"@daily":    "%s %s * * *",
	"@midnight": "%s 0 * * *",
	"@weekly":   "%s %s * * 0",
	"@monthly":  "%s %s 1 * *",
	"@annually": "%s %s 1 1 *",
	"@yearly":   "%s %s 1 1 *",
}

func snapshotIsScheduledNow(spec string, subjectID int64) bool {
	var result = false

	specs := buildCronSpecs(spec, subjectID)
	for _, curSpec := range specs {
		isNow, err := cronSpecIsNow(curSpec)
		if err == nil && isNow {
			result = true
		}
	}

	return result
}

func buildCronSpecs(spec string, subjectID int64) []string {
	var result []string

	if strings.Contains(spec, ", ") {
		for _, curSpec := range util.SplitNTrimSpace(spec, ",", -1, true) {

			result = append(result, getCronSyntax(curSpec, subjectID))
		}
	} else {
		result = append(result, getCronSyntax(spec, subjectID))
	}

	return result
}

func getCronSyntax(spec string, subjectID int64) string {
	alias, isAlias := SnapshotScheduleAliases[strings.ToLower(spec)]
	if isAlias {
		obfuscatedMinute, obfuscatedHour := getObfuscatedTimeValuesForSubject(subjectID)

		if strings.Count(alias, "%s") > 1 {
			return fmt.Sprintf(alias, obfuscatedMinute, obfuscatedHour)
		} else {
			return fmt.Sprintf(alias, obfuscatedMinute)
		}
	}

	return spec
}

func getObfuscatedTimeValuesForSubject(subjectID int64) (string, string) {
	var minuteResult = "0"
	var hourResult = "0"

	minSequence, minSequenceErr := util.GenerateSequenceInt64(0, 60, 1)
	min, minErr := util.GetStableRandomInt64FromList(subjectID, minSequence)
	if minErr == nil && minSequenceErr == nil {
		minuteResult = strconv.FormatInt(min, 10)
	}

	hourSequence, hourSequenceErr := util.GenerateSequenceInt64(0, 24, 1)
	hour, hourErr := util.GetStableRandomInt64FromList(subjectID, hourSequence)
	if hourErr == nil && hourSequenceErr == nil {
		hourResult = strconv.FormatInt(hour, 10)
	}

	return minuteResult, hourResult
}

func cronSpecIsNow(spec string) (bool, error) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return false, fmt.Errorf("Could not parse cron '%s'", spec)
	}

	// Check if it's time to snapshot
	now := time.Now()

	// Truncate the time now back to the start of the minute.
	// This is neded because the cron scheduler will add a minute to the scheduled time
	// and we don't want the next scheduled time to roll over to the next minute and break
	// the time comparison below.
	now = now.Truncate(time.Minute)

	// Calculate the next scheduled time based on the snapshots.schedule
	// pattern and the time now.
	next := sched.Next(now)

	if !now.Add(time.Minute).Equal(next) {
		return false, nil
	}

	return true, nil
}
