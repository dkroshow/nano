# A saved file keeps its identity when a reboot renumbers its volume

Date: 2026-10-22

Status: accepted.

## Decision

Nano still identifies a saved provider file by its device number and inode,
and every existing check keeps its meaning: the store's replaced check, the
catalog's skip, append and touch checks, and the evidence reader. A file with
the same inode but another device number (`history.DeviceRenumbered`) still
counts as replaced, unless the catalog's scan adopts it first
(`history.Store.AdoptSourceIdentity`).

Adoption needs all three:
- the file has not shrunk;
- its own transcript checkpoint names this source and generation, without an
  error;
- the last bytes it parsed are unchanged (the saved tail digest,
  `TranscriptSourceTailMatches`).

Adoption records the new device number on the row. The generation stays, so
the file is not read again, and every check, the evidence reader included,
sees the same file again. Anything unproven stays replaced and is read once.

## Why

The 2026-10-22 macOS 26.7.1 reboot renumbered the data volume: `st_dev` went
from 16777267 to 16777266 for every file, while inodes stayed the same.
Nano then treated every unchanged transcript as replaced:
- any sweep re-read about 8,000 files, which took hours;
- staging could not finish within its guard;
- source evidence reads failed for every untouched file.

Operator: "This was a one off, however it may happen again, so the software
should know what to do in this case" (2026-10-22). It is rare, so the fix is
the smallest one that handles it automatically.

## Not done, with reasons

- **No persistent volume id.** The tail digest already proves an unchanged
  file, so a volume UUID would add a schema column and a platform call for no
  more safety.
- **The device check stays.** A changed device with no proof is still a
  replaced file: inode numbers can repeat on another volume.
- **The live store was corrected once**, by an ordinary full re-read
  (Refresh) after the reboot, not by hand edits.
