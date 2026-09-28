// Folder selection helpers shared by the picker and the job editor.
//
// The API identifies a folder by a drive id plus a folder id, while the job
// stores the human readable path next to them (so a job list can name the folders
// without walking the tree). Both halves are produced together by the picker, so
// they travel as one value.

/** FolderSelection is what the folder browser returns to the job editor. */
export interface FolderSelection {
  driveId: string;
  /** folderId is empty at the root of the drive. */
  folderId: string;
  /** folderPath is the display path; `/` stands for the root. */
  folderPath: string;
}

/** ROOT_PATH is how the API path of a drive root is displayed. */
export const ROOT_PATH = '/';

/** pathFrom renders the display path of a breadcrumb built from the root. */
export function pathFrom(names: string[]): string {
  const parts = names.filter((name) => name !== '');
  return parts.length === 0 ? ROOT_PATH : parts.join('/');
}

/** crumbLabel is the label of one breadcrumb segment. */
export function crumbLabel(name: string, rootLabel: string): string {
  return name === '' ? rootLabel : name;
}
