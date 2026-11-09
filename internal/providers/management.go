package providers

import "context"

// ManagementStep is one progress report of a management operation. Providers call
// the ProgressFunc handed to them once per action they take, which is what turns
// a long administrative task into an auditable journal (see
// models.ManagementTaskStep). Name is a short machine-ish label, Detail the human
// context, Items and Bytes the counters the step moved.
type ManagementStep struct {
	Name   string
	Status string
	Detail string
	Items  int
	Bytes  int64
}

// Step outcomes a provider may report. They mirror models.ManagementStep* one for
// one; the constants are repeated here so a provider never imports the models
// package for them.
const (
	StepSuccess = "success"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// ProgressFunc receives the steps of an operation as it runs. An implementation
// must be called from the goroutine running the operation, which is why it is a
// plain callback rather than a channel.
type ProgressFunc func(step ManagementStep)

// ManagementResult is the summary an operation answers at the end: a readable
// message, the item and byte counters it moved and a small set of key/value
// details the history screen can show (a new drive id, a site url, …).
type ManagementResult struct {
	Message string
	Items   int
	Bytes   int64
	Data    map[string]string
}

// SharedDriveManager is implemented by providers whose files can be moved into a
// shared drive. Google Drive exposes shared drives as first-class objects; Graph
// has document libraries instead, which is why the interface is Google-only.
type SharedDriveManager interface {
	// ListSharedDrives answers the shared drives the account can see.
	ListSharedDrives(ctx context.Context, creds Credentials, tokens *Tokens) ([]Drive, error)
	// CreateSharedDrive creates a shared drive named name and answers it.
	CreateSharedDrive(ctx context.Context, creds Credentials, tokens *Tokens, name string) (Drive, error)
	// MoveIntoSharedDrive moves every top level item of sourceDriveID into
	// targetFolderID of targetDriveID (empty targetFolderID means the root of
	// the shared drive), reporting each item through progress. A folder is moved
	// with its whole subtree, so only the top level has to be enumerated.
	MoveIntoSharedDrive(ctx context.Context, creds Credentials, tokens *Tokens, sourceDriveID, targetDriveID, targetFolderID string, progress ProgressFunc) (ManagementResult, error)
}

// DirectoryUser is one account an administrative operation can name: the address
// and display name the form offers, plus whether it is a guest.
type DirectoryUser struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Guest bool   `json:"guest"`
}

// DirectoryGroup is one group an administrative operation can target.
type DirectoryGroup struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// DirectoryReader is implemented by providers that can list the users and groups
// of the directory behind the connection. It feeds the selects of the Admin
// operations forms, so an operator picks an existing account or group instead of
// typing an address that a typo can turn into a new object.
type DirectoryReader interface {
	ListDirectoryUsers(ctx context.Context, creds Credentials, tokens *Tokens) ([]DirectoryUser, error)
	ListDirectoryGroups(ctx context.Context, creds Credentials, tokens *Tokens) ([]DirectoryGroup, error)
}

// MailboxDelegate is one delegate of a mailbox: the address and the provider's
// verification status.
type MailboxDelegate struct {
	Email  string `json:"email"`
	Status string `json:"status"`
}

// ArchiveUserInput is the request of the user → group archive: the departing
// user, the group that should take over their address and an optional alias the
// user's own mailbox is renamed to (freeing the address for the group).
type ArchiveUserInput struct {
	UserEmail    string
	GroupEmail   string
	GroupName    string
	MemberEmail  string
	ArchiveEmail string
}

// DirectoryArchiver is implemented by providers that can archive a departing
// user into a group: rename the mailbox to free the address, create the group
// that keeps it and add the successor as a member.
type DirectoryArchiver interface {
	ArchiveUserToGroup(ctx context.Context, creds Credentials, tokens *Tokens, input ArchiveUserInput, progress ProgressFunc) (ManagementResult, error)
}

// DelegateInput is one delegate operation. Action is add, remove or list.
type DelegateInput struct {
	Mailbox  string
	Delegate string
	Action   string
}

// MailboxDelegator is implemented by providers whose mailboxes can carry
// delegates (Google Gmail settings).
type MailboxDelegator interface {
	ManageDelegates(ctx context.Context, creds Credentials, tokens *Tokens, input DelegateInput, progress ProgressFunc) (ManagementResult, error)
	// ListDelegates answers the current delegates of one mailbox, which is what
	// the Admin operations form shows before an add or a remove.
	ListDelegates(ctx context.Context, creds Credentials, tokens *Tokens, mailbox string) ([]MailboxDelegate, error)
}

// CopyMailboxInput is the request of the mailbox copy: copy the messages of the
// mailbox Source into Target, at most Max messages (0 means a provider default).
type CopyMailboxInput struct {
	Source string
	Target string
	Max    int
}

// MailboxCopier is implemented by providers whose mailboxes can be copied message
// by message into another mailbox.
type MailboxCopier interface {
	CopyMailbox(ctx context.Context, creds Credentials, tokens *Tokens, input CopyMailboxInput, progress ProgressFunc) (ManagementResult, error)
}

// CalendarTransferInput is the request of the calendar transfer: give Target
// ownership of the calendars of Source.
type CalendarTransferInput struct {
	Source string
	Target string
}

// CalendarTransferrer is implemented by providers whose calendars can change
// hands.
type CalendarTransferrer interface {
	TransferCalendars(ctx context.Context, creds Credentials, tokens *Tokens, input CalendarTransferInput, progress ProgressFunc) (ManagementResult, error)
}

// FolderSizeInput is the request of the folder size report: walk drive DriveID
// (empty means the account's own drive).
type FolderSizeInput struct {
	DriveID string
}

// FolderSizeReporter is implemented by providers that can walk a drive and report
// the size of every folder (the storage report of the Admin operations screen).
type FolderSizeReporter interface {
	ReportFolderSizes(ctx context.Context, creds Credentials, tokens *Tokens, input FolderSizeInput, progress ProgressFunc) (ManagementResult, error)
}

// SiteCopyInput is the request of the OneDrive → SharePoint operation: either
// copy into an existing drive (TargetDriveID) or create a new site first
// (SiteName/SiteDisplayName) and copy into the drive Graph provisions for it.
type SiteCopyInput struct {
	SiteName        string
	SiteDisplayName string
	TargetDriveID   string
	TargetFolderID  string
}

// SiteContentCopier is implemented by providers that can copy the account's own
// drive into a SharePoint document library.
type SiteContentCopier interface {
	CopyDriveToSite(ctx context.Context, creds Credentials, tokens *Tokens, input SiteCopyInput, progress ProgressFunc) (ManagementResult, error)
}
