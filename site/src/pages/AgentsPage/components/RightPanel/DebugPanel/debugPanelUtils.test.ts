import { coerceStepResponse } from "./debugPanelUtils";

describe("coerceStepResponse", () => {
	it("keeps tool-result content emitted in normalized response parts", () => {
		const response = coerceStepResponse({
			content: [
				{
					type: "tool-result",
					tool_call_id: "call-1",
					tool_name: "search_docs",
					result: {
						matches: ["model.go", "debugPanelUtils.ts"],
					},
				},
			],
		});

		const parsed = JSON.parse(response.content);
		expect(parsed).toEqual({
			matches: ["model.go", "debugPanelUtils.ts"],
		});
		expect(response.toolCalls).toEqual([]);
		expect(response.usage).toEqual({});
	});
});
