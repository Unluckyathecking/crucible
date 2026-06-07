import { auth } from "@/auth";
import { redirect } from "next/navigation";
import { ensureCustomer, getStripeCustomerId } from "@/lib/db";
import BillingPageContent from "./billing-content";

export const dynamic = "force-dynamic";

export default async function BillingPage() {
  const session = await auth();
  if (!session?.user?.email) {
    redirect("/login");
  }
  const customer = await ensureCustomer(session.user.email);
  const stripeCustomerId = await getStripeCustomerId(customer.id);

  return (
    <BillingPageContent
      planId={customer.plan_id}
      hasStripeCustomer={stripeCustomerId !== null}
    />
  );
}
