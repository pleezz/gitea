// Copyright 2018 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"fmt"

	"code.gitea.io/gitea/modules/log"
	api "code.gitea.io/gitea/modules/structs"

	"xorm.io/xorm"
)

// IssueAssignees saves all issue assignees
type IssueAssignees struct {
	ID         int64 `xorm:"pk autoincr"`
	AssigneeID int64 `xorm:"INDEX"`
	IssueID    int64 `xorm:"INDEX"`
}

// This loads all assignees of an issue
func (issue *Issue) loadAssignees(e Engine) (err error) {
	// Reset maybe preexisting assignees
	issue.Assignees = []*User{}

	err = e.Table("`user`").
		Join("INNER", "issue_assignees", "assignee_id = `user`.id").
		Where("issue_assignees.issue_id = ?", issue.ID).
		Find(&issue.Assignees)

	if err != nil {
		return err
	}

	// Check if we have at least one assignee and if yes put it in as `Assignee`
	if len(issue.Assignees) > 0 {
		issue.Assignee = issue.Assignees[0]
	}

	return
}

// GetAssigneesByIssue returns everyone assigned to that issue
func GetAssigneesByIssue(issue *Issue) (assignees []*User, err error) {
	return getAssigneesByIssue(x, issue)
}

func getAssigneesByIssue(e Engine, issue *Issue) (assignees []*User, err error) {
	err = issue.loadAssignees(e)
	if err != nil {
		return assignees, err
	}

	return issue.Assignees, nil
}

// IsUserAssignedToIssue returns true when the user is assigned to the issue
func IsUserAssignedToIssue(issue *Issue, user *User) (isAssigned bool, err error) {
	return isUserAssignedToIssue(x, issue, user)
}

func isUserAssignedToIssue(e Engine, issue *Issue, user *User) (isAssigned bool, err error) {
	return e.Get(&IssueAssignees{IssueID: issue.ID, AssigneeID: user.ID})
}

// DeleteNotPassedAssignee deletes all assignees who aren't passed via the "assignees" array
func DeleteNotPassedAssignee(issue *Issue, doer *User, assignees []*User) (err error) {
	var found bool

	for _, assignee := range issue.Assignees {

		found = false
		for _, alreadyAssignee := range assignees {
			if assignee.ID == alreadyAssignee.ID {
				found = true
				break
			}
		}

		if !found {
			// This function also does comments and hooks, which is why we call it seperatly instead of directly removing the assignees here
			if _, _, err := issue.ToggleAssignee(doer, assignee.ID); err != nil {
				return err
			}
		}
	}

	return nil
}

// MakeAssigneeList concats a string with all names of the assignees. Useful for logs.
func MakeAssigneeList(issue *Issue) (assigneeList string, err error) {
	err = issue.loadAssignees(x)
	if err != nil {
		return "", err
	}

	for in, assignee := range issue.Assignees {
		assigneeList += assignee.Name

		if len(issue.Assignees) > (in + 1) {
			assigneeList += ", "
		}
	}
	return
}

// ClearAssigneeByUserID deletes all assignments of an user
func clearAssigneeByUserID(sess *xorm.Session, userID int64) (err error) {
	_, err = sess.Delete(&IssueAssignees{AssigneeID: userID})
	return
}

// ToggleAssignee changes a user between assigned and not assigned for this issue, and make issue comment for it.
func (issue *Issue) ToggleAssignee(doer *User, assigneeID int64) (removed bool, comment *Comment, err error) {
	sess := x.NewSession()
	defer sess.Close()

	if err := sess.Begin(); err != nil {
		return false, nil, err
	}

	removed, comment, err = issue.toggleAssignee(sess, doer, assigneeID, false)
	if err != nil {
		return false, nil, err
	}

	if err := sess.Commit(); err != nil {
		return false, nil, err
	}

	go HookQueue.Add(issue.RepoID)

	return removed, comment, nil
}

func (issue *Issue) toggleAssignee(sess *xorm.Session, doer *User, assigneeID int64, isCreate bool) (removed bool, comment *Comment, err error) {
	removed, err = toggleUserAssignee(sess, issue, assigneeID)
	if err != nil {
		return false, nil, fmt.Errorf("UpdateIssueUserByAssignee: %v", err)
	}

	// Repo infos
	if err = issue.loadRepo(sess); err != nil {
		return false, nil, fmt.Errorf("loadRepo: %v", err)
	}

	// Comment
	comment, err = createAssigneeComment(sess, doer, issue.Repo, issue, assigneeID, removed)
	if err != nil {
		return false, nil, fmt.Errorf("createAssigneeComment: %v", err)
	}

	// if pull request is in the middle of creation - don't call webhook
	if isCreate {
		return removed, comment, err
	}

	if issue.IsPull {
		mode, _ := accessLevelUnit(sess, doer, issue.Repo, UnitTypePullRequests)

		if err = issue.loadPullRequest(sess); err != nil {
			return false, nil, fmt.Errorf("loadPullRequest: %v", err)
		}
		issue.PullRequest.Issue = issue
		apiPullRequest := &api.PullRequestPayload{
			Index:       issue.Index,
			PullRequest: issue.PullRequest.apiFormat(sess),
			Repository:  issue.Repo.innerAPIFormat(sess, mode, false),
			Sender:      doer.APIFormat(),
		}
		if removed {
			apiPullRequest.Action = api.HookIssueUnassigned
		} else {
			apiPullRequest.Action = api.HookIssueAssigned
		}
		// Assignee comment triggers a webhook
		if err := prepareWebhooks(sess, issue.Repo, HookEventPullRequest, apiPullRequest); err != nil {
			log.Error("PrepareWebhooks [is_pull: %v, remove_assignee: %v]: %v", issue.IsPull, removed, err)
			return false, nil, err
		}
	} else {
		mode, _ := accessLevelUnit(sess, doer, issue.Repo, UnitTypeIssues)

		apiIssue := &api.IssuePayload{
			Index:      issue.Index,
			Issue:      issue.apiFormat(sess),
			Repository: issue.Repo.innerAPIFormat(sess, mode, false),
			Sender:     doer.APIFormat(),
		}
		if removed {
			apiIssue.Action = api.HookIssueUnassigned
		} else {
			apiIssue.Action = api.HookIssueAssigned
		}
		// Assignee comment triggers a webhook
		if err := prepareWebhooks(sess, issue.Repo, HookEventIssues, apiIssue); err != nil {
			log.Error("PrepareWebhooks [is_pull: %v, remove_assignee: %v]: %v", issue.IsPull, removed, err)
			return false, nil, err
		}
	}
	return removed, comment, nil
}

// toggles user assignee state in database
func toggleUserAssignee(e *xorm.Session, issue *Issue, assigneeID int64) (removed bool, err error) {

	// Check if the user exists
	assignee, err := getUserByID(e, assigneeID)
	if err != nil {
		return false, err
	}

	// Check if the submitted user is already assigned, if yes delete him otherwise add him
	var i int
	for i = 0; i < len(issue.Assignees); i++ {
		if issue.Assignees[i].ID == assigneeID {
			break
		}
	}

	assigneeIn := IssueAssignees{AssigneeID: assigneeID, IssueID: issue.ID}

	toBeDeleted := i < len(issue.Assignees)
	if toBeDeleted {
		issue.Assignees = append(issue.Assignees[:i], issue.Assignees[i:]...)
		_, err = e.Delete(assigneeIn)
		if err != nil {
			return toBeDeleted, err
		}
	} else {
		issue.Assignees = append(issue.Assignees, assignee)
		_, err = e.Insert(assigneeIn)
		if err != nil {
			return toBeDeleted, err
		}
	}

	return toBeDeleted, nil
}

// MakeIDsFromAPIAssigneesToAdd returns an array with all assignee IDs
func MakeIDsFromAPIAssigneesToAdd(oneAssignee string, multipleAssignees []string) (assigneeIDs []int64, err error) {

	// Keeping the old assigning method for compatibility reasons
	if oneAssignee != "" {

		// Prevent double adding assignees
		var isDouble bool
		for _, assignee := range multipleAssignees {
			if assignee == oneAssignee {
				isDouble = true
				break
			}
		}

		if !isDouble {
			multipleAssignees = append(multipleAssignees, oneAssignee)
		}
	}

	// Get the IDs of all assignees
	assigneeIDs, err = GetUserIDsByNames(multipleAssignees, false)

	return
}
