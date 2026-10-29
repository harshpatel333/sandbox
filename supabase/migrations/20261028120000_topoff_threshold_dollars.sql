-- Auto Topoff thresholds are whole dollars from $1; amounts stay in $5 steps.
ALTER TABLE billing_prepaid_auto_recharge DROP CONSTRAINT billing_prepaid_auto_recharge_threshold_cents_check,
 ADD CONSTRAINT billing_prepaid_auto_recharge_threshold_cents_check
  CHECK (threshold_cents >= 100 AND threshold_cents % 100 = 0 AND threshold_cents <= 100000000);
ALTER TABLE billing_plan_version DROP CONSTRAINT billing_plan_version_topoff_threshold_cents_check,
 ADD CONSTRAINT billing_plan_version_topoff_threshold_cents_check
  CHECK (topoff_threshold_cents BETWEEN 100 AND 1000000 AND topoff_threshold_cents % 100 = 0);
