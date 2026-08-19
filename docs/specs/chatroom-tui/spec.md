---
status: draft
date: 2026-08-19
implements: [ADR-0001]
---

# SPEC-0001: Unified Chatroom TUI for Multi-Harness Agent Output

## Overview

This specification defines a unified, read-only "chatroom" style TUI that aggregates live output from all supported agent harnesses (Claude Code, Codex, Crush, OpenCode, Pi) into a single chronological stream. Each harness appears as a distinct "user" with a username (e.g., `@crush-signal`) and color. Tool calls, tool results, and user messages render as chat messages. An activity feed panel provides a summary timeline.

See ADR-0001 for the architectural decision context.

## Requirements

### Requirement: Multi-Harness Event Aggregation

The TUI SHALL connect to `tail.Watcher` configured with `tail.DefaultAdapters()` and consume the unified `Event` channel from all five harness adapters.

#### Scenario: Watcher Connection

- **WHEN** the TUI starts
- **THEN** it SHALL create a `tail.Watcher` with `tail.DefaultWatchConfig()` and `tail.DefaultAdapters()`
- **THEN** it SHALL call `Watcher.Start(ctx)` and consume events from `Watcher.Events()`

#### Scenario: Event Reception

- **WHEN** any harness produces a new tool call, tool result, or user message
- **THEN** the corresponding adapter SHALL emit a `tail.Event` through the watcher
- **THEN** the TUI SHALL receive the event within 2 seconds (watcher poll interval)

### Requirement: Chronological Event Ordering

The TUI SHALL display all events from all harnesses in a single unified stream ordered by event timestamp (oldest first, newest at bottom).

#### Scenario: Event Ordering

- **WHEN** events arrive from multiple harnesses concurrently
- **THEN** the TUI SHALL merge them into a single timeline sorted by `Event.Classified.Timestamp` (or `Event.ReceivedAt` as fallback)
- **THEN** events with identical timestamps SHALL be ordered by harness name for determinism

### Requirement: Harness Identity Display

Each harness SHALL have a distinct visual identity in the chatroom.

#### Scenario: Harness Username

- **WHEN** rendering an event from a harness
- **THEN** the TUI SHALL display the harness username prefix:
  - `claude-code` → `@claude-code`
  - `codex` → `@codex`
  - `crush` → `@crush-signal`
  - `opencode` → `@opencode`
  - `pi` → `@pi`

#### Scenario: Harness Color

- **WHEN** rendering an event from a harness
- **THEN** the username and message SHALL use a distinct color per harness:
  - `@claude-code`: Purple (#BB86FC)
  - `@codex`: Green (#03DAC6)
  - `@crush-signal`: Orange (#FFB74D)
  - `@opencode`: Blue (#64B5F6)
  - `@pi`: Pink (#F06292)

### Requirement: Tool Call Rendering

Tool calls SHALL render as chat messages with action type badges.

#### Scenario: Tool Call Message

- **WHEN** a `classify.Event` with `Action` ≠ `ActionOther` is received
- **THEN** the TUI SHALL render a message:
  - Username in harness color (e.g., `@crush-signal`)
  - Action badge: `[SEARCH]`, `[READ]`, `[EDIT]`, `[EXEC]`, `[VERIFY]`, `[OTHER]`
  - Tool name (e.g., `grep`, `bash`, `read_file`)
  - Summary from `Event.Summary` (truncated to 80 chars)
  - Timestamp in HH:MM:SS format

#### Scenario: Tool Call with Targets

- **WHEN** a tool call has `Targets` (file paths)
- **THEN** the TUI SHALL display targets as clickable/navigable references below the message
- **THEN** targets SHALL show touch rank indicators (edit > read > hit)

### Requirement: Tool Result Rendering

Tool results SHALL render as follow-up messages linked to their tool call.

#### Scenario: Tool Result Message

- **WHEN** a `classify.Event` represents a tool result (paired with a prior tool call)
- **THEN** the TUI SHALL render a message:
  - Same username/harness color
  - Status badge: `[OK]` (success) or `[ERROR]` (failure)
  - Duration badge (e.g., `1.2s`)
  - Truncated output preview (first 120 chars)
  - Visual indentation/grouping with the originating tool call

### Requirement: User Message Rendering (Marks)

User messages from `classify.Mark` SHALL render as chat messages.

#### Scenario: User Message

- **WHEN** a `tail.Event` contains `Marks` (user messages, compactions, subagent launches)
- **THEN** the TUI SHALL render each mark as a message:
  - Username in harness color
  - Mark type badge: `[USER]`, `[COMPACTION]`, `[SUBAGENT]`
  - Mark content (truncated to 200 chars)
  - Timestamp

### Requirement: Activity Feed Panel

A secondary panel SHALL display a condensed activity timeline.

#### Scenario: Activity Feed

- **WHEN** the TUI is running
- **THEN** a side panel SHALL show:
  - One line per event: `HH:MM:SS @harness action/tool summary`
  - Color-coded by harness
  - Clicking/navigating to an entry scrolls the main chat to that event
  - Auto-scrolls to latest by default

### Requirement: Keyboard Navigation

The TUI SHALL support standard keyboard controls.

#### Scenario: Scrolling

- **WHEN** user presses `Up`/`Down` or `k`/`j`
- **THEN** the chat viewport SHALL scroll line by line
- **WHEN** user presses `Page Up`/`Page Down`
- **THEN** the chat viewport SHALL scroll by page

#### Scenario: Pause/Resume

- **WHEN** user presses `Space` or `p`
- **THEN** auto-scroll to new events SHALL toggle (pause/resume)
- **THEN** a pause indicator SHALL be visible in the status bar

#### Scenario: Filter by Harness

- **WHEN** user presses `1`–`5` or `f`
- **THEN** the TUI SHALL filter events to show only the selected harness(es)
- **WHEN** user presses `0` or `a`
- **THEN** the TUI SHALL show all harnesses (no filter)

#### Scenario: Quit

- **WHEN** user presses `q` or `Ctrl+C`
- **THEN** the TUI SHALL gracefully stop the watcher and exit

### Requirement: Terminal Resize Handling

The TUI SHALL handle terminal resize events gracefully.

#### Scenario: Resize

- **WHEN** the terminal window is resized
- **THEN** the TUI SHALL reflow layout (chat panel, activity panel, status bar)
- **THEN** scroll position SHALL be preserved relative to bottom

### Requirement: High-Contrast Mode

The TUI SHALL support a high-contrast color mode for accessibility.

#### Scenario: High Contrast

- **WHEN** the `CHATROOM_HIGH_CONTRAST=1` environment variable is set
- **THEN** the TUI SHALL use high-contrast colors (white/black, bold/underline for badges)
- **THEN** harness distinction SHALL use text prefixes + formatting instead of color alone

## Security Requirements

This spec is NOT web-facing — it is a local terminal application with no network exposure. No security requirements section is injected.

## Accessibility Requirements

This spec IS UI-facing (terminal UI). The following accessibility requirements apply per WCAG 2.1 AA adapted for TUI:

### Requirement: Keyboard Operability

All TUI functions SHALL be operable via keyboard alone (no mouse required).

### Requirement: Color Not Sole Indicator

Harness identity SHALL NOT rely on color alone — username prefix (`@harness`) and/or formatting (bold, underline) MUST always be present.

### Requirement: High Contrast Mode

As specified in "High-Contrast Mode" requirement above.

### Requirement: Screen Reader Compatible Output

All text output SHALL be plain text with semantic structure (no raw escape sequences in logs) so screen readers can process scrollback.

### Requirement: Focus Management

When switching between chat panel and activity panel (via `Tab`), focus SHALL move visibly and predictably.

### Requirement: Reduced Motion

When `CHATROOM_REDUCED_MOTION=1` is set, auto-scroll animations SHALL be disabled (instant jump).

## Backend Quality Requirements

This spec involves backend quality concerns (concurrency, error handling):

### Requirement: Concurrency Safety

The TUI's event processing SHALL use proper context propagation for cancellation. The watcher runs in a goroutine; the bubbletea event loop runs in another. Channel communication between them SHALL be bounded and respect context cancellation.

### Requirement: Error Handling Standards

All errors from `tail.Watcher`, adapters, and bubbletea SHALL be wrapped with context. Silent error swallowing SHALL NOT occur — errors SHALL be logged to a debug file and/or displayed in a status area.

## Design

This spec is paired with `design.md` which covers the architecture, decisions, and implementation details.