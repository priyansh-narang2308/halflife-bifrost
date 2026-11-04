import { NoPermissionView } from "@/components/noPermissionView";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { createFileRoute } from "@tanstack/react-router";
import HalflifePage from "./page";

// Reports hold the audited answers, so they are read with the Logs permission.
function RouteComponent() {
	const hasAccess = useRbac(RbacResource.Logs, RbacOperation.View);
	if (!hasAccess) {
		return <NoPermissionView entity="Halflife reports" />;
	}
	return <HalflifePage />;
}

export const Route = createFileRoute("/workspace/halflife")({
	component: RouteComponent,
});
