BEGIN;
DO $$
DECLARE c billing_commercial_contract; p billing_contract_payment; b billing_contract_provider_binding;
 s timestamptz:='2025-02-28 00:00:00+00'; e timestamptz:='2025-03-31 00:00:00+00'; ok boolean; ordinary_team uuid; ordinary_contract uuid;
BEGIN
 SELECT contract.* INTO STRICT c FROM billing_commercial_contract contract JOIN pair_test_case tc ON tc.contract_id=contract.id WHERE tc.kind='monthly';
 SELECT * INTO STRICT b FROM billing_contract_provider_binding WHERE contract_id=c.id;
 -- Usage calendar ends March31; fee calendar ends March28. Both identity and
 -- calendar must come from the immutable fee binding when admitting cash.
 BEGIN
  INSERT INTO billing_contract_payment(team_id,contract_id,invoice_id,payment_reference,customer_id,subscription_id,base_price_id,service_period_start,service_period_end,base_amount_paid_cents,invoice_amount_paid_cents,invoice_amount_remaining_cents,currency,paid_at,verified_at,evidence)
  VALUES(c.team_id,c.id,'in_wrong','ref_wrong',c.customer_id,c.subscription_id,c.base_price_id,s,'2025-03-28',120000,120000,0,'usd',s,clock_timestamp(),'{}');
  RAISE EXCEPTION 'wrong fee identity accepted' USING ERRCODE='assert_failure';
 EXCEPTION WHEN raise_exception THEN
  IF SQLERRM<>'base payment does not match commercial terms' THEN RAISE; END IF;
 END;
 BEGIN
  INSERT INTO billing_contract_payment(team_id,contract_id,invoice_id,payment_reference,customer_id,subscription_id,base_price_id,service_period_start,service_period_end,base_amount_paid_cents,invoice_amount_paid_cents,invoice_amount_remaining_cents,currency,paid_at,verified_at,evidence)
  VALUES(c.team_id,c.id,'in_wrong','ref_wrong',c.customer_id,b.fee_subscription_id,c.base_price_id,s,e,120000,120000,0,'usd',s,clock_timestamp(),'{}');
  RAISE EXCEPTION 'usage calendar substituted for fee calendar' USING ERRCODE='assert_failure';
 EXCEPTION WHEN raise_exception THEN
  IF SQLERRM<>'base payment service period differs from contract cadence' THEN RAISE; END IF;
 END;
 INSERT INTO billing_contract_payment(team_id,contract_id,invoice_id,payment_reference,customer_id,subscription_id,base_price_id,service_period_start,service_period_end,base_amount_paid_cents,invoice_amount_paid_cents,invoice_amount_remaining_cents,currency,paid_at,verified_at,evidence)
 VALUES(c.team_id,c.id,'in_pair','ref_pair',c.customer_id,b.fee_subscription_id,c.base_price_id,s,'2025-03-28',120000,120000,0,'usd',s,clock_timestamp(),
  '{"invoice":{"id":"in_pair","lines":{"data":[{"parent":{"subscription_item_details":{"subscription_item":"si_fee"}}}]}}}') RETURNING * INTO p;
 INSERT INTO billing_pro_payment_verification(invoice_id,review_required,evidence) VALUES(p.invoice_id,false,'{}');
 c.status:='active'; c.collection_has_overdue_balance:=false; c.collection_checked_at:=clock_timestamp(); c.collection_valid_until:=clock_timestamp()+interval '1 minute';
 c.collection_evidence:=jsonb_build_object('gate','finalized_invoice_gate_v1','period_start',s,'period_end',e);
 IF billing_contract_eligible(c,s,e) THEN RAISE EXCEPTION 'unmapped fee receipt authorized allowance'; END IF;
 INSERT INTO billing_provider_pair_monthly_payment(payment_id,team_id,contract_id,invoice_id,fee_item_id,fee_period_start,fee_period_end,period_start,period_end,amount_cents)
 VALUES(p.id,c.team_id,c.id,p.invoice_id,b.fee_item_id,s,'2025-03-28',s,e,100000);
 IF billing_contract_eligible(c,s,e) IS NOT TRUE THEN RAISE EXCEPTION 'mapped receipt lost allowance authority'; END IF;
 SET LOCAL ROLE service_role;
 IF billing_contract_eligible(c,s,e) IS NOT TRUE THEN RAISE EXCEPTION 'backend role lost mapped authority'; END IF;
 RESET ROLE;
 SET LOCAL ROLE anon;
 BEGIN
  PERFORM billing_contract_eligible(c,s,e);
  RAISE EXCEPTION 'anon acquired financial authority' USING ERRCODE='assert_failure';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 RESET ROLE;
 SET LOCAL ROLE authenticated;
 BEGIN
  PERFORM billing_contract_eligible(c,s,e);
  RAISE EXCEPTION 'authenticated acquired financial authority' USING ERRCODE='assert_failure';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 RESET ROLE;

 IF billing_contract_eligible(c,e,'2025-04-30') THEN RAISE EXCEPTION 'receipt reused outside mapped allowance'; END IF;
 INSERT INTO billing_provider_pair_revocation(contract_id,team_id,identity) VALUES(c.id,c.team_id,b.binding->'identity');
 IF billing_contract_eligible(c,s,e) THEN RAISE EXCEPTION 'revoked pair authorized allowance'; END IF;
 -- Actual paired initial-waiver authority requires active assignment and fresh
 -- observation, and remains revoked even when all ordinary collection checks pass.
 SELECT contract.* INTO STRICT c FROM billing_commercial_contract contract JOIN pair_test_case tc ON tc.contract_id=contract.id WHERE tc.kind='waiver';
 SELECT * INTO STRICT b FROM billing_contract_provider_binding WHERE contract_id=c.id;
 s:=c.entitlement_anchor; e:=billing_anniversary_boundary(s,1);
 c.collection_has_overdue_balance:=false; c.collection_checked_at:=clock_timestamp(); c.collection_valid_until:=clock_timestamp()+interval '1 minute';
 c.collection_evidence:=jsonb_build_object('gate','finalized_invoice_gate_v1','period_start',s,'period_end',e);
 IF billing_contract_eligible(c,s,e) IS NOT TRUE THEN RAISE EXCEPTION 'paired waived allowance lost authority'; END IF;
 IF billing_contract_eligible(c,s,c.ends_at+interval '1 day') THEN RAISE EXCEPTION 'waiver exceeded its coverage'; END IF;
 UPDATE billing_provider_pair_waiver SET authority_checked_at=clock_timestamp(),authority_valid_until=clock_timestamp()+interval '0.1 seconds' WHERE contract_id=c.id;
 PERFORM pg_sleep(0.15);
 IF billing_contract_eligible(c,s,e) THEN RAISE EXCEPTION 'stale waiver authorized allowance'; END IF;
 UPDATE billing_provider_pair_waiver SET authority_checked_at=clock_timestamp(),authority_valid_until=clock_timestamp()+interval '50 seconds' WHERE contract_id=c.id;
 UPDATE billing_provider_pair_waiver SET revoked_at=clock_timestamp() WHERE contract_id=c.id;
 IF billing_contract_eligible(c,s,e) THEN RAISE EXCEPTION 'revoked waiver authorized allowance'; END IF;
 -- The same ahead-schema function must still admit the new single-subscription
 -- Pro terms. Initial coupon/bridge cash belongs to the upgrade journal, not
 -- an ordinary full-price base renewal receipt.
 ordinary_team:=gen_random_uuid(); ordinary_contract:=gen_random_uuid();
 INSERT INTO team(id,name,home_region) VALUES(ordinary_team,'ordinary ' || ordinary_team::text,'use');
 INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,commercial_billing_anchor)
 VALUES(ordinary_team,'cus_ordinary','sub_ordinary','2025-02-01');
 INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
 VALUES(ordinary_contract,ordinary_team,'pro_monthly','2025-02-01','cus_ordinary','sub_ordinary','price_250',25000,1,'metered_usage',25000,'monthly','2025-02-01');
 INSERT INTO billing_contract_payment(team_id,contract_id,invoice_id,payment_reference,customer_id,subscription_id,base_price_id,service_period_start,service_period_end,base_amount_paid_cents,invoice_amount_paid_cents,invoice_amount_remaining_cents,currency,paid_at,verified_at,evidence)
 VALUES(ordinary_team,ordinary_contract,'in_ordinary','ref_ordinary','cus_ordinary','sub_ordinary','price_250','2025-02-01','2025-03-01',25000,25000,0,'usd','2025-02-01',clock_timestamp(),'{"fixture":true}') RETURNING * INTO p;
 INSERT INTO billing_pro_payment_verification(invoice_id,review_required,evidence) VALUES(p.invoice_id,false,'{}');
 SELECT * INTO c FROM billing_commercial_contract WHERE id=ordinary_contract;
 c.status:='active'; c.collection_has_overdue_balance:=false; c.collection_checked_at:=clock_timestamp(); c.collection_valid_until:=clock_timestamp()+interval '1 minute';
 c.collection_evidence:=jsonb_build_object('gate','finalized_invoice_gate_v1','period_start',p.service_period_start,'period_end',p.service_period_end);
 IF billing_contract_eligible(c,p.service_period_start,p.service_period_end) IS NOT TRUE THEN RAISE EXCEPTION 'ordinary Pro lost authority on paired schema'; END IF;
END $$;
ROLLBACK;
SELECT 'paired authority preserved';
