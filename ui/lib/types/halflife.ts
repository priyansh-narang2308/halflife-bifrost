// Halflife reports, as served by /api/halflife (plugins/halflife/report.go,
// list.go and metrics.go).

export interface HalflifeReportSummary {
	id: string;
	request_id: string;
	status: string;
	outcome?: string;
	verdict: string;
	created_at: string;
	completed_at?: string;
	tenant_virtual_key_id: string;
	provider: string;
	model: string;
	claims: number;
	claims_checked: number;
	credits: number;
}

export interface HalflifeReportList {
	reports: HalflifeReportSummary[];
	next_before?: string;
}

export interface HalflifeQuote {
	text: string;
	url?: string;
	verified: boolean;
	flagged?: boolean;
}

export interface HalflifeSource {
	title: string;
	url: string;
	published_date?: string;
	snippet: string;
	flagged?: boolean;
	flag_reason?: string;
}

export interface HalflifeClaim {
	text: string;
	span: string;
	category?: string;
	temporal_scope?: string;
	volatility_band?: string;
	priority?: string;
	priority_reason?: string;
	status: string;
	status_reason?: string;
	verdict?: string;
	verdict_reason?: string;
	observed_value?: string;
	quote?: HalflifeQuote;
	history_quote?: HalflifeQuote;
	other_quote?: HalflifeQuote;
	failure?: string;
	evidence?: { credits: number; reused: boolean; sources: HalflifeSource[] };
}

export interface HalflifeReport {
	id: string;
	request_id: string;
	status: string;
	outcome?: string;
	outcome_reason?: string;
	verdict: string;
	created_at: string;
	tenant: { virtual_key_id?: string; virtual_key_name?: string };
	answer: { provider: string; model: string; question?: string; text: string; text_redacted?: boolean; cache_id?: string };
	claims: HalflifeClaim[];
	error?: string;
}

export interface HalflifeTenantUsage {
	tenant_virtual_key_id: string;
	tenant_virtual_key_name?: string;
	audits: number;
	claims_checked: number;
	tavily_credits: number;
	tavily_usd: number;
	llm_usd: number;
}

export interface HalflifeDependency {
	state: string;
	reason?: string;
}

export interface HalflifeTenantSpend {
	cap_usd: number;
	spent_usd: number;
	resets_at?: string;
}

export interface HalflifeUsage {
	mode: string;
	tenants: HalflifeTenantUsage[];
	dependencies: Record<string, HalflifeDependency>;
	spend: Record<string, HalflifeTenantSpend>;
	requests: {
		Captured: number;
		CachedHits: number;
		Invalidated: number;
		Expired: number;
		TombstoneBypasses: number;
		TombstoneReuses: number;
	};
}
