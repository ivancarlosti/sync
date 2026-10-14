// Drive context helpers shared by the folder picker and the screens that show a
// stored selection.
//
// A folder is identified by a drive id, but only the provider knows what that
// drive *is*: the account's own drive, a shared drive, or one library of one
// SharePoint site. The picker resolves that when a folder is chosen (it is what
// `providers.Drive` carries on the server) and the job and the audit store the
// parts next to the id, so a list names a location without a provider round
// trip. The helpers below are the vocabulary of that label, so the picker, the
// job list and the audit history all read the same way.
import { type Composer } from 'vue-i18n';

import type { ProviderName } from '@/lib/api';

/** DriveIdentity is the parts of a drive that label it in the UI. */
export interface DriveIdentity {
  name?: string;
  /** kind is the provider's drive type (see providers.Drive). */
  kind?: string;
  /** owner carries the drive URL, which is where the SharePoint site lives. */
  owner?: string;
}

/**
 * siteOf names the SharePoint site a library lives in, which is what tells two
 * libraries apart: every site ships a default library literally called
 * "Documents", so the name alone would show one indistinguishable row per site.
 * The site is the segment after /sites or /teams in the drive URL; a URL that
 * carries none — the account's own OneDrive under /personal — contributes
 * nothing, because that drive is already unique. The site is skipped when it
 * merely repeats the drive name, so a searched site is not labelled twice.
 */
export function siteOf(drive: DriveIdentity): string {
  if (!drive.owner) {
    return '';
  }
  try {
    const segments = new URL(drive.owner).pathname.split('/').filter((part) => part !== '');
    const marker = segments.findIndex((part) => ['sites', 'teams'].includes(part.toLowerCase()));
    const place = marker >= 0 ? segments[marker + 1] : undefined;
    if (!place || place.toLowerCase() === (drive.name ?? '').toLowerCase()) {
      return '';
    }
    return place;
  } catch {
    return '';
  }
}

/**
 * isOneDriveKind marks the account's own drive: "personal" on a personal
 * account, "business" on a work/school one. Every other Microsoft kind is a
 * SharePoint library, which is what keeps the work OneDrive out of the library
 * group.
 */
export function isOneDriveKind(kind: string | undefined): boolean {
  return kind === 'personal' || kind === 'business';
}

/** isSharedDrive marks a Google shared drive, the counterpart of My Drive. */
export function isSharedDrive(kind: string | undefined): boolean {
  return kind === 'shared';
}

/**
 * driveFamilyKey is the catalog key of the family of a drive: what tells an
 * OneDrive from a SharePoint library, and My Drive from a shared drive. The
 * provider disambiguates "personal", which Google uses for My Drive and
 * Microsoft for the account's own OneDrive, and "shared" is Google-only, so it
 * is recognised even when the account behind a row is gone.
 */
export function driveFamilyKey(provider: ProviderName | '', drive: DriveIdentity): string {
  if (isSharedDrive(drive.kind)) {
    return 'browser.sharedDrive';
  }
  if (provider === 'google') {
    return 'browser.myDrive';
  }
  if (isOneDriveKind(drive.kind)) {
    return 'browser.oneDrive';
  }
  return 'browser.sharePointLibrary';
}

/**
 * driveLabel names the drive a selection sits in, as one short phrase:
 * "My Drive", "Shared drive · Finance", "OneDrive", "SharePoint library ·
 * Documents · marketing". The name only travels when it adds something: My
 * Drive and OneDrive are single roots the family already names, while a shared
 * drive carries a real name — and a library needs both its name and its site,
 * since every site calls its default library "Documents".
 *
 * A drive whose name and kind are both unknown (a job or an audit stored before
 * these columns existed) is not labelled at all: the family would be a guess,
 * and an empty label is what keeps those rows rendering exactly as before.
 */
export function driveLabel(
  t: Composer['t'],
  provider: ProviderName | '',
  drive: DriveIdentity,
): string {
  const name = (drive.name ?? '').trim();
  const kind = (drive.kind ?? '').trim();
  if (name === '' && kind === '') {
    return '';
  }
  const family = driveFamilyKey(provider, drive);
  const parts = [t(family)];
  if (family === 'browser.sharedDrive') {
    parts.push(name);
  }
  if (family === 'browser.sharePointLibrary') {
    parts.push(name, siteOf(drive));
  }
  return parts.filter((part) => part !== '').join(' · ');
}

/**
 * JobDriveColumns is the flat drive columns a job carries for both ends (the
 * stored SyncJob and the JobPayload that writes it have the same shape here).
 */
export interface JobDriveColumns {
  source_drive_name: string;
  source_drive_kind: string;
  source_drive_owner: string;
  destination_drive_name: string;
  destination_drive_kind: string;
  destination_drive_owner: string;
}

/** driveFromJob names one end of a job as a drive identity. */
export function driveFromJob(job: JobDriveColumns, which: 'source' | 'destination'): DriveIdentity {
  if (which === 'source') {
    return {
      name: job.source_drive_name,
      kind: job.source_drive_kind,
      owner: job.source_drive_owner,
    };
  }
  return {
    name: job.destination_drive_name,
    kind: job.destination_drive_kind,
    owner: job.destination_drive_owner,
  };
}

/** AuditDriveColumns is the flat drive columns an audit carries for its root. */
export interface AuditDriveColumns {
  drive_name: string;
  drive_kind: string;
  drive_owner: string;
}

/** driveFromAudit names the root of an audit as a drive identity. */
export function driveFromAudit(run: AuditDriveColumns): DriveIdentity {
  return { name: run.drive_name, kind: run.drive_kind, owner: run.drive_owner };
}
