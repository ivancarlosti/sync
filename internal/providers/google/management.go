package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// The Admin operations of the Google provider: moving a My Drive into a shared
// drive, archiving a departing user into a group, copying a mailbox, transferring
// calendars and managing Gmail delegates.
//
// Everything is issued through the same doJSON helper as the Drive calls, which
// is what keeps the request-URL guard (a request can only leave for
// googleapis.com), the retry policy and the timeout in one place.
const (
	// driveAPIBase creates and lists shared drives.
	driveAPIBase = "https://www.googleapis.com/drive/v3"
	// adminAPIBase is the Admin SDK Directory API (groups, members, users).
	adminAPIBase = "https://admin.googleapis.com/admin/directory/v1"
	// gmailAPIBase is the Gmail API (message copy, delegates).
	gmailAPIBase = "https://gmail.googleapis.com/gmail/v1"
	// calendarAPIBase is the Calendar API v3.
	calendarAPIBase = "https://www.googleapis.com/calendar/v3"

	// mailboxCopyDefaultMax bounds a mailbox copy when the operator does not say
	// otherwise: a first run over a busy mailbox must not read every message of
	// the last decade in one task.
	mailboxCopyDefaultMax = 200
	// mailboxCopyMax is the hard ceiling of a single copy task.
	mailboxCopyMax = 5000
)

// Compile time guards: the operations registry hands these out as interfaces, so a
// drifted signature must fail the build rather than a task at runtime.
var (
	_ providers.SharedDriveManager  = (*Provider)(nil)
	_ providers.DirectoryArchiver   = (*Provider)(nil)
	_ providers.MailboxCopier       = (*Provider)(nil)
	_ providers.CalendarTransferrer = (*Provider)(nil)
)

// isAPIStatus reports whether err is an *apiError answered with the given HTTP
// status. A 409 on a create call means "already exists", which several operations
// treat as a skip instead of a failure.
func isAPIStatus(err error, status int) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == status
}

// pathSegment escapes a value that goes into a URL path segment (a mailbox
// address, a group key, a calendar id). The result stays inside the alphabet the
// request-URL guard accepts.
func pathSegment(value string) string {
	return url.PathEscape(strings.TrimSpace(value))
}

// driveSize parses the Drive string size, answering 0 when it is empty or not a
// number (a folder carries no size).
func driveSize(raw string) int64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// counterOr parses a small counter kept in the result details, answering 0 for a
// missing or malformed value.
func counterOr(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}

// reportStep forwards a step to the caller, tolerating a nil progress func so a
// provider test can call an operation without wiring a journal.
func reportStep(progress providers.ProgressFunc, name, status, detail string, items int, bytes int64) {
	if progress == nil {
		return
	}
	progress(providers.ManagementStep{Name: name, Status: status, Detail: detail, Items: items, Bytes: bytes})
}

// ListSharedDrives implements providers.SharedDriveManager.
func (p *Provider) ListSharedDrives(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.Drive, error) {
	client := p.client(tokens)
	base := driveAPIBase + "/drives"
	base = withParam(base, "pageSize", "100")
	base = withParam(base, "fields", "nextPageToken,drives(id,name)")
	drives := make([]providers.Drive, 0, 16)
	pageToken := ""
	for {
		target := base
		if pageToken != "" {
			target = withParam(base, "pageToken", pageToken)
		}
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Drives        []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"drives"`
		}
		if err := p.doJSON(ctx, client, http.MethodGet, target, nil, &page); err != nil {
			return nil, err
		}
		for _, drive := range page.Drives {
			drives = append(drives, providers.Drive{ID: drive.ID, Name: drive.Name, Kind: "shared"})
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			return drives, nil
		}
	}
}

// CreateSharedDrive implements providers.SharedDriveManager. Drive requires a
// unique requestId per creation, which is also what makes a retry idempotent.
func (p *Provider) CreateSharedDrive(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, name string) (providers.Drive, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return providers.Drive{}, fmt.Errorf("%w: a shared drive name is required", providers.ErrInvalidIdentifier)
	}
	client := p.client(tokens)
	target := driveAPIBase + "/drives"
	target = withParam(target, "requestId", fmt.Sprintf("sync-%d", time.Now().UnixNano()))
	target = withParam(target, "fields", "id,name")
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := p.doJSON(ctx, client, http.MethodPost, target, map[string]string{"name": name}, &created); err != nil {
		return providers.Drive{}, err
	}
	return providers.Drive{ID: created.ID, Name: created.Name, Kind: "shared"}, nil
}

// MoveIntoSharedDrive implements providers.SharedDriveManager. It enumerates the
// top level of the source drive and re-parents every item into the target shared
// drive; a folder carries its subtree with it, so one pass moves everything.
func (p *Provider) MoveIntoSharedDrive(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, sourceDriveID, targetDriveID, targetFolderID string, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	if !identifierPattern.MatchString(targetDriveID) {
		return providers.ManagementResult{}, invalidIdentifierError("shared drive id", targetDriveID)
	}
	if sourceDriveID != "" && !optionalIdentifierPattern.MatchString(sourceDriveID) {
		return providers.ManagementResult{}, invalidIdentifierError("drive id", sourceDriveID)
	}
	targetParent := rootParent(targetDriveID, providers.DriveRoot)
	if targetFolderID != "" {
		if !identifierPattern.MatchString(targetFolderID) {
			return providers.ManagementResult{}, invalidIdentifierError("target folder id", targetFolderID)
		}
		targetParent = targetFolderID
	}
	sourceRoot := rootParent(sourceDriveID, providers.DriveRoot)
	if !identifierPattern.MatchString(sourceRoot) {
		return providers.ManagementResult{}, invalidIdentifierError("folder id", sourceRoot)
	}
	client := p.client(tokens)

	base := withDriveScope(listURL("/files", "nextPageToken,files("+itemFields+")"), sourceDriveID)
	base = withParam(base, "q", fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(sourceRoot, "'", "\\'")))
	base = withParam(base, "pageSize", "1000")

	result := providers.ManagementResult{Data: map[string]string{
		"target_drive_id":  targetDriveID,
		"target_folder_id": targetParent,
	}}
	pageToken := ""
	for {
		target := base
		if pageToken != "" {
			target = withParam(base, "pageToken", pageToken)
		}
		var page struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveFile `json:"files"`
		}
		if err := p.doJSON(ctx, client, http.MethodGet, target, nil, &page); err != nil {
			return result, err
		}
		for _, file := range page.Files {
			if file.Trashed {
				continue
			}
			if !identifierPattern.MatchString(file.ID) {
				reportStep(progress, file.Name, providers.StepFailed, "invalid item id", 0, 0)
				result.Data["failed"] = strconv.Itoa(counterOr(result.Data["failed"]) + 1)
				continue
			}
			parent := sourceRoot
			if len(file.Parents) > 0 {
				parent = file.Parents[0]
			}
			patch := driveAPIBase + "/files/" + file.ID
			patch = withParam(patch, "addParents", targetParent)
			patch = withParam(patch, "removeParents", parent)
			patch = withParam(patch, "supportsAllDrives", "true")
			patch = withParam(patch, "fields", "id,name")
			size := driveSize(file.Size)
			if err := p.doJSON(ctx, client, http.MethodPatch, patch, map[string]any{}, nil); err != nil {
				reportStep(progress, file.Name, providers.StepFailed, err.Error(), 0, size)
				result.Data["failed"] = strconv.Itoa(counterOr(result.Data["failed"]) + 1)
				continue
			}
			reportStep(progress, file.Name, providers.StepSuccess, "moved", 1, size)
			result.Items++
			result.Bytes += size
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			break
		}
	}
	result.Message = fmt.Sprintf("moved %d item(s) into the shared drive", result.Items)
	return result, nil
}

// ArchiveUserToGroup implements providers.DirectoryArchiver: rename the departing
// mailbox to free its address, create the group that keeps the address and add
// the successor as a member. A group or membership that already exists is a
// skip, not a failure, so a re-run converges instead of dying on 409.
func (p *Provider) ArchiveUserToGroup(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, input providers.ArchiveUserInput, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	user := strings.TrimSpace(input.UserEmail)
	group := strings.TrimSpace(input.GroupEmail)
	if user == "" || group == "" {
		return providers.ManagementResult{}, fmt.Errorf("%w: a user email and a group email are required", providers.ErrInvalidIdentifier)
	}
	client := p.client(tokens)
	result := providers.ManagementResult{Data: map[string]string{"group_email": group}}

	if archive := strings.TrimSpace(input.ArchiveEmail); archive != "" {
		target := adminAPIBase + "/users/" + pathSegment(user)
		if err := p.doJSON(ctx, client, http.MethodPut, target, map[string]string{"primaryEmail": archive}, nil); err != nil {
			reportStep(progress, "rename mailbox", providers.StepFailed, err.Error(), 0, 0)
			return result, fmt.Errorf("google: renaming %s to %s failed: %w", user, archive, err)
		}
		reportStep(progress, "rename mailbox", providers.StepSuccess, user+" → "+archive, 1, 0)
		result.Items++
	}

	name := strings.TrimSpace(input.GroupName)
	if name == "" {
		name = group
	}
	create := adminAPIBase + "/groups"
	err := p.doJSON(ctx, client, http.MethodPost, create, map[string]string{"email": group, "name": name}, nil)
	switch {
	case err == nil:
		reportStep(progress, "create group", providers.StepSuccess, group, 1, 0)
		result.Items++
	case isAPIStatus(err, http.StatusConflict):
		reportStep(progress, "create group", providers.StepSkipped, "the group already exists", 0, 0)
	default:
		reportStep(progress, "create group", providers.StepFailed, err.Error(), 0, 0)
		return result, fmt.Errorf("google: creating the group %s failed: %w", group, err)
	}

	if member := strings.TrimSpace(input.MemberEmail); member != "" {
		add := adminAPIBase + "/groups/" + pathSegment(group) + "/members"
		err := p.doJSON(ctx, client, http.MethodPost, add, map[string]string{"email": member, "role": "MEMBER"}, nil)
		switch {
		case err == nil:
			reportStep(progress, "add member", providers.StepSuccess, member, 1, 0)
			result.Items++
		case isAPIStatus(err, http.StatusConflict):
			reportStep(progress, "add member", providers.StepSkipped, member+" is already a member", 0, 0)
		default:
			reportStep(progress, "add member", providers.StepFailed, err.Error(), 0, 0)
			result.Data["failed"] = "1"
		}
	}
	result.Message = "the address was archived into the group"
	return result, nil
}

// ListDirectoryUsers implements providers.DirectoryReader: every account of the
// domain, which is the source/destination select of the Admin operations forms.
func (p *Provider) ListDirectoryUsers(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.DirectoryUser, error) {
	client := p.client(tokens)
	target := adminBase + "/users"
	target = withParam(target, "customer", customerKey)
	target = withParam(target, "maxResults", "500")
	target = withParam(target, "projection", "basic")
	users := make([]providers.DirectoryUser, 0, 256)
	pageToken := ""
	for {
		request := target
		if pageToken != "" {
			request = withParam(request, "pageToken", pageToken)
		}
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Users         []struct {
				PrimaryEmail string `json:"primaryEmail"`
				Name         struct {
					FullName string `json:"fullName"`
				} `json:"name"`
				Suspended bool `json:"suspended"`
			} `json:"users"`
		}
		if err := p.doJSON(ctx, client, http.MethodGet, request, nil, &page); err != nil {
			return nil, err
		}
		for _, user := range page.Users {
			email := textCell(user.PrimaryEmail)
			if email == "" || user.Suspended {
				continue
			}
			users = append(users, providers.DirectoryUser{Email: email, Name: textCell(user.Name.FullName)})
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			return users, nil
		}
	}
}

// ListDirectoryGroups implements providers.DirectoryReader: every group of the
// domain, which is the archive destination select.
func (p *Provider) ListDirectoryGroups(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.DirectoryGroup, error) {
	client := p.client(tokens)
	target := adminBase + "/groups"
	target = withParam(target, "customer", customerKey)
	target = withParam(target, "maxResults", "200")
	groups := make([]providers.DirectoryGroup, 0, 128)
	pageToken := ""
	for {
		request := target
		if pageToken != "" {
			request = withParam(request, "pageToken", pageToken)
		}
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Groups        []struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"groups"`
		}
		if err := p.doJSON(ctx, client, http.MethodGet, request, nil, &page); err != nil {
			return nil, err
		}
		for _, group := range page.Groups {
			email := textCell(group.Email)
			if email == "" {
				continue
			}
			groups = append(groups, providers.DirectoryGroup{Email: email, Name: textCell(group.Name)})
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			return groups, nil
		}
	}
}

// CopyMailbox implements providers.MailboxCopier: read the raw content of every
// message of the source mailbox and insert it into the target mailbox. Reading a
// mailbox other than the connected account's own requires domain-wide delegation
// on the OAuth client; without it Google answers 403 and the task fails with that
// explanation (see docs/management.md).
func (p *Provider) CopyMailbox(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, input providers.CopyMailboxInput, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	source := strings.TrimSpace(input.Source)
	if source == "" {
		source = "me"
	}
	targetMailbox := strings.TrimSpace(input.Target)
	if targetMailbox == "" {
		return providers.ManagementResult{}, fmt.Errorf("%w: a target mailbox is required", providers.ErrInvalidIdentifier)
	}
	max := input.Max
	if max <= 0 {
		max = mailboxCopyDefaultMax
	}
	if max > mailboxCopyMax {
		max = mailboxCopyMax
	}
	client := p.client(tokens)
	result := providers.ManagementResult{Data: map[string]string{"source": source, "target": targetMailbox}}

	listBase := gmailAPIBase + "/users/" + pathSegment(source) + "/messages"
	listBase = withParam(listBase, "maxResults", "100")
	pageToken := ""
	for result.Items+counterOr(result.Data["failed"]) < max {
		target := listBase
		if pageToken != "" {
			target = withParam(listBase, "pageToken", pageToken)
		}
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Messages      []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		if err := p.doJSON(ctx, client, http.MethodGet, target, nil, &page); err != nil {
			return result, err
		}
		for _, message := range page.Messages {
			if result.Items+counterOr(result.Data["failed"]) >= max {
				break
			}
			if !identifierPattern.MatchString(message.ID) {
				continue
			}
			raw, err := p.messageRaw(ctx, client, source, message.ID)
			if err != nil {
				reportStep(progress, message.ID, providers.StepFailed, err.Error(), 0, 0)
				result.Data["failed"] = strconv.Itoa(counterOr(result.Data["failed"]) + 1)
				continue
			}
			if raw == "" {
				reportStep(progress, message.ID, providers.StepSkipped, "the message has no raw form", 0, 0)
				continue
			}
			insert := gmailAPIBase + "/users/" + pathSegment(targetMailbox) + "/messages"
			if err := p.doJSON(ctx, client, http.MethodPost, insert, map[string]string{"raw": raw}, nil); err != nil {
				reportStep(progress, message.ID, providers.StepFailed, err.Error(), 0, 0)
				result.Data["failed"] = strconv.Itoa(counterOr(result.Data["failed"]) + 1)
				continue
			}
			reportStep(progress, message.ID, providers.StepSuccess, "copied", 1, 0)
			result.Items++
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			break
		}
	}
	result.Message = fmt.Sprintf("copied %d message(s)", result.Items)
	return result, nil
}

// messageRaw reads the RFC 822 form of one message, base64url encoded, which is
// what the insert endpoint expects back.
func (p *Provider) messageRaw(ctx context.Context, client *http.Client, source, id string) (string, error) {
	target := gmailAPIBase + "/users/" + pathSegment(source) + "/messages/" + id
	target = withParam(target, "format", "raw")
	var message struct {
		Raw string `json:"raw"`
	}
	if err := p.doJSON(ctx, client, http.MethodGet, target, nil, &message); err != nil {
		return "", err
	}
	return message.Raw, nil
}

// TransferCalendars implements providers.CalendarTransferrer: for every calendar
// the source owns, insert an owner ACL entry for the target. That is the
// ownership handover Google supports through the API; the events stay on the
// calendar, which keeps the history intact.
func (p *Provider) TransferCalendars(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, input providers.CalendarTransferInput, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	source := strings.TrimSpace(input.Source)
	if source == "" {
		source = "me"
	}
	target := strings.TrimSpace(input.Target)
	if target == "" {
		return providers.ManagementResult{}, fmt.Errorf("%w: a target mailbox is required", providers.ErrInvalidIdentifier)
	}
	client := p.client(tokens)
	result := providers.ManagementResult{Data: map[string]string{"target": target}}

	listBase := calendarAPIBase + "/users/" + pathSegment(source) + "/calendarList"
	listBase = withParam(listBase, "maxResults", "250")
	pageToken := ""
	for {
		pageTarget := listBase
		if pageToken != "" {
			pageTarget = withParam(listBase, "pageToken", pageToken)
		}
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				ID         string `json:"id"`
				Summary    string `json:"summary"`
				AccessRole string `json:"accessRole"`
				Primary    bool   `json:"primary"`
			} `json:"items"`
		}
		if err := p.doJSON(ctx, client, http.MethodGet, pageTarget, nil, &page); err != nil {
			return result, err
		}
		for _, calendar := range page.Items {
			if calendar.ID == "" {
				continue
			}
			acl := calendarAPIBase + "/calendars/" + pathSegment(calendar.ID) + "/acl"
			body := map[string]any{
				"role":  "owner",
				"scope": map[string]string{"type": "user", "value": target},
			}
			err := p.doJSON(ctx, client, http.MethodPost, acl, body, nil)
			name := calendar.Summary
			if name == "" {
				name = calendar.ID
			}
			switch {
			case err == nil:
				reportStep(progress, name, providers.StepSuccess, "ownership granted to "+target, 1, 0)
				result.Items++
			case isAPIStatus(err, http.StatusConflict):
				reportStep(progress, name, providers.StepSkipped, "the target already has access", 0, 0)
			default:
				reportStep(progress, name, providers.StepFailed, err.Error(), 0, 0)
				result.Data["failed"] = strconv.Itoa(counterOr(result.Data["failed"]) + 1)
			}
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			break
		}
	}
	result.Message = fmt.Sprintf("transferred %d calendar(s)", result.Items)
	return result, nil
}
