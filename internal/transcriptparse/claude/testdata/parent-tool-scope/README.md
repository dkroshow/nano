# Parent tool scope

Constructed from the documented stream-json envelope, not a captured run.
The parent and nested tool stream deliberately reuse API message ID `shared`
and block index 0. `parent_tool_use_id` is their distinguishing native scope.
The child result must complete only the child turn. Parent content remains
`Parent tail`, child content `Child`; usage contributions remain 7 and 3.
No native child session ID or native turn ID is invented.

Executable expectations: TestClaudeParentToolScope in scope_test.go.
