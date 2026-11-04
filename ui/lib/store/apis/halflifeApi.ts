import type { HalflifeReport, HalflifeReportList, HalflifeUsage } from "@/lib/types/halflife";
import { baseApi } from "./baseApi";

export const halflifeApi = baseApi.injectEndpoints({
	endpoints: (builder) => ({
		getHalflifeReports: builder.query<HalflifeReportList, { verdict?: string; limit?: number }>({
			query: (params) => ({ url: "/halflife/reports", params }),
		}),
		getHalflifeReport: builder.query<HalflifeReport, string>({
			query: (id) => ({ url: `/halflife/reports/${encodeURIComponent(id)}` }),
		}),
		getHalflifeUsage: builder.query<HalflifeUsage, void>({
			query: () => ({ url: "/halflife/usage" }),
		}),
	}),
});

export const { useGetHalflifeReportsQuery, useGetHalflifeReportQuery, useGetHalflifeUsageQuery } = halflifeApi;
