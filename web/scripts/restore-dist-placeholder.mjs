// Vite empties `dist` on every build, which would delete the placeholder that
// `//go:embed all:dist` needs in a fresh clone. Run automatically as the
// `postbuild` script so `go build ./...` keeps working after `npm run build`.
import { writeFileSync } from 'node:fs';

const placeholder = new URL('../dist/.gitkeep', import.meta.url);

writeFileSync(placeholder, '');
console.log('web/scripts: restored dist/.gitkeep');
