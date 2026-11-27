**logic:** Critical architectural issue: calling move functions directly bypasses `async_execute_controller_command()`, which violates the architecture documented in AGENTS.md line 62-63: "All entities use this for proper locking". This breaks command serialization via `_command_lock` and position polling during movement (`_async_poll_positions_during_movement`).

The move functions call `write_command()` which uses the bed controller's `_ble_lock`, but that's a different lock for low-level GATT writes. The `_command_lock` in the coordinator serializes all commands to prevent concurrent operations.

Without using `async_execute_controller_command()`:
1. Concurrent position reads during movement won't happen (breaks real-time feedback)
2. The final position read after movement won't happen
3. Multiple number entity changes could execute concurrently
4. `disconnect_after_command` behavior won't work correctly

<details><summary>Prompt To Fix With AI</summary>

`````markdown
This is a comment left during a code review.
Path: custom_components/adjustable_bed/coordinator.py
Line: 1355:1359

Comment:
**logic:** Critical architectural issue: calling move functions directly bypasses `async_execute_controller_command()`, which violates the architecture documented in AGENTS.md line 62-63: "All entities use this for proper locking". This breaks command serialization via `_command_lock` and position polling during movement (`_async_poll_positions_during_movement`).

The move functions call `write_command()` which uses the bed controller's `_ble_lock`, but that's a different lock for low-level GATT writes. The `_command_lock` in the coordinator serializes all commands to prevent concurrent operations.

Without using `async_execute_controller_command()`:
1. Concurrent position reads during movement won't happen (breaks real-time feedback)
2. The final position read after movement won't happen
3. Multiple number entity changes could execute concurrently
4. `disconnect_after_command` behavior won't work correctly

How can I resolve this? If you propose a fix, please make it concise.
`````
</details>