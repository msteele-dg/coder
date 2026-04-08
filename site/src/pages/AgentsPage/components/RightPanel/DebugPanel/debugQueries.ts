// Debug-specific query factories live here rather than in the
// shared site/src/api/queries/chats.ts to keep the main chat
// queries module focused on core chat operations.

import { API } from "#/api/api";
import type * as TypesGen from "#/api/typesGenerated";

// ---------------------------------------------------------------------------
// Terminal status detection (shared by list and detail queries).
// ---------------------------------------------------------------------------

const debugRunTerminalStatuses = new Set(["completed", "error", "interrupted"]);

const debugRunRefetchInterval = (
	run: Pick<TypesGen.ChatDebugRun, "status"> | undefined,
	hasError?: boolean,
): number | false => {
	if (hasError) {
		return false;
	}
	if (run?.status && debugRunTerminalStatuses.has(run.status.toLowerCase())) {
		return false;
	}
	return 5_000;
};

// ---------------------------------------------------------------------------
// Query factories.
// ---------------------------------------------------------------------------

const chatDebugRunsKey = (chatId: string) =>
	["chats", chatId, "debug-runs"] as const;

export const chatDebugRuns = (chatId: string) => ({
	queryKey: chatDebugRunsKey(chatId),
	queryFn: () => API.experimental.getChatDebugRuns(chatId),
	refetchInterval: ({
		state,
	}: {
		state: {
			data?: TypesGen.ChatDebugRunSummary[] | undefined;
			status: string;
		};
	}): number | false => {
		if (state.status === "error") {
			return false;
		}
		// Keep polling at a consistent foreground cadence while the
		// Debug tab is open. A slower terminal-state interval delays
		// discovery of newly-started runs until the user switches tabs.
		return 5_000;
	},
	refetchIntervalInBackground: false,
});

export const chatDebugRun = (chatId: string, runId: string) => ({
	queryKey: [...chatDebugRunsKey(chatId), runId] as const,
	queryFn: () => API.experimental.getChatDebugRun(chatId, runId),
	refetchInterval: ({
		state,
	}: {
		state: { data: TypesGen.ChatDebugRun | undefined; status: string };
	}) => debugRunRefetchInterval(state.data, state.status === "error"),
	refetchIntervalInBackground: false,
});
