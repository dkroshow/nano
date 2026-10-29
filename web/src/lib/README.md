# UI areas

Components and their supporting logic live together by responsibility. Tests
and fixtures sit beside the behavior they check. App-level stores, routing,
and server-event coordination remain in `web/src/`.

| Area | Owns |
| --- | --- |
| [transcript](transcript/) | Saved and live message rendering, the prompt textarea, markdown, file views and transcript navigation. |
| [explorer](explorer/) | Sidebar and Thread Explorer lists, row projection, details, names, stars and close/resume controls. |
| [panes](panes/) | Workspace panes, tabs, responsive layout and keyboard navigation. |
| [composer](composer/) | Pending sends, turn queues, interaction responses and new-session options. |
| [mail](mail/) | Agent inboxes, global mail and message display formatting. |
| [status](status/) | Connection and usage displays, settings and session inspection. |
| [shared](shared/) | Small lifecycle, formatting, focus and stream helpers used across areas. |

Import the owning file directly; there are no forwarding modules or barrel
exports to keep in sync.
