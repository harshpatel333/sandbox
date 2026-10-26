-- Synthetic identities, but real upstream tables, immutable guards and mappings.
CREATE TABLE pair_test_case (contract_id uuid, team_id uuid, assignment_id uuid, kind text);
DO $$
DECLARE t uuid:=gen_random_uuid(); a uuid:=gen_random_uuid(); c uuid:=gen_random_uuid();
 anchor timestamptz:='2024-01-31 00:00:00+00'; start_at timestamptz:='2025-02-28 00:00:00+00';
 identity jsonb; terms jsonb; binding jsonb;
BEGIN
 INSERT INTO team(id,name,home_region) VALUES(t,'pair compatibility ' || t::text,'use');
 INSERT INTO team_feature_flag(team_id,key,enabled) VALUES(t,'billing_commercial_offer_assignments_enabled',true);
 INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,commercial_billing_anchor)
 VALUES(t,'cus_pair','sub_usage',anchor);
 INSERT INTO billing_commercial_offer_assignment(id,team_id,approval_id,company_identity,home_region,offer_key,offer_version,idempotency_key,rate_card_key,rate_ids,pricing_at,resource_rates,request,assigned_by,reason,reference)
 VALUES(a,t,gen_random_uuid(),'fixture','use','yc_2026',1,'initial','payg',ARRAY[gen_random_uuid()],clock_timestamp(),'[]','{}',gen_random_uuid(),'fixture','fixture');
 INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
 VALUES(c,t,'pro_monthly',start_at,'cus_pair','sub_usage','price_fee',120000,1,'metered_usage',100000,'monthly',anchor);
 identity:=jsonb_build_object('operation_id',gen_random_uuid(),'team_id',t,'assignment_id',a,'acceptance_id',gen_random_uuid(),
  'approval_semantic_revision',0,'terms_digest',repeat('a',64),'provider_terms_digest',repeat('b',64),
  'merchant','acct_fixture','live',false,'api_version','2026-01-28.clover');
 terms:=jsonb_build_object('fee_price_id','price_fee','fee_amount_cents',120000,'fee_months',1,'currency','usd');
 binding:=jsonb_build_object('contract_id',c,'identity',identity,'terms',terms,'customer_id','cus_pair',
  'usage_subscription_id','sub_usage','fee_subscription_id','sub_fee','fee_item_id','si_fee',
  'anchor',anchor,'fee_anchor',start_at,'starts_at',start_at,'ends_at',NULL);
 INSERT INTO billing_contract_provider_binding(contract_id,team_id,assignment_id,acceptance_id,approval_semantic_revision,merchant,live_mode,api_version,terms_digest,provider_terms_digest,customer_id,usage_subscription_id,fee_subscription_id,fee_item_id,anchor,fee_anchor,starts_at,binding,evidence,verified_at)
 VALUES(c,t,a,(identity->>'acceptance_id')::uuid,0,'acct_fixture',false,'2026-01-28.clover',repeat('a',64),repeat('b',64),
 'cus_pair','sub_usage','sub_fee','si_fee',anchor,start_at,start_at,binding,'{}',clock_timestamp());
 INSERT INTO pair_test_case VALUES(c,t,a,'monthly');
END $$;

DO $$
DECLARE t uuid:=gen_random_uuid(); a uuid:=gen_random_uuid(); c uuid:=gen_random_uuid();
 anchor timestamptz:=date_trunc('second',clock_timestamp()-interval '1 day'); start_at timestamptz:=anchor; finish timestamptz:=billing_anniversary_boundary(anchor,12);
 identity jsonb; terms jsonb; binding jsonb;
BEGIN
 INSERT INTO team(id,name,home_region) VALUES(t,'pair compatibility ' || t::text,'use');
 INSERT INTO team_feature_flag(team_id,key,enabled) VALUES(t,'billing_commercial_offer_assignments_enabled',true);
 INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,commercial_billing_anchor)
 VALUES(t,'cus_waiver','sub_usage_waiver',anchor);
 INSERT INTO billing_commercial_offer_assignment(id,team_id,approval_id,company_identity,home_region,offer_key,offer_version,idempotency_key,rate_card_key,rate_ids,pricing_at,resource_rates,request,assigned_by,reason,reference)
 VALUES(a,t,gen_random_uuid(),'fixture','use','yc_2026',1,'initial','payg',ARRAY[gen_random_uuid()],clock_timestamp(),'[]','{}',gen_random_uuid(),'fixture','fixture');
 INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,ends_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
 VALUES(c,t,'yc_2026',start_at,finish,'cus_waiver','sub_usage_waiver','price_fee',1200000,12,'metered_usage',100000,'scheduled',anchor);
 identity:=jsonb_build_object('operation_id',gen_random_uuid(),'team_id',t,'assignment_id',a,'acceptance_id',gen_random_uuid(),
  'approval_semantic_revision',0,'terms_digest',repeat('a',64),'provider_terms_digest',repeat('b',64),
  'merchant','acct_fixture','live',false,'api_version','2026-01-28.clover');
 terms:=jsonb_build_object('fee_price_id','price_fee','fee_amount_cents',1200000,'fee_months',12,'resources','[]'::jsonb,'currency','usd');
 binding:=jsonb_build_object('contract_id',c,'identity',identity,'terms',terms,'customer_id','cus_waiver',
  'usage_subscription_id','sub_usage_waiver','fee_subscription_id','sub_fee_waiver','fee_item_id','si_fee',
  'anchor',anchor,'fee_anchor',start_at,'starts_at',start_at,'ends_at',finish);
 INSERT INTO billing_contract_provider_binding(contract_id,team_id,assignment_id,acceptance_id,approval_semantic_revision,merchant,live_mode,api_version,terms_digest,provider_terms_digest,customer_id,usage_subscription_id,fee_subscription_id,fee_item_id,anchor,fee_anchor,starts_at,ends_at,binding,evidence,verified_at)
 VALUES(c,t,a,(identity->>'acceptance_id')::uuid,0,'acct_fixture',false,'2026-01-28.clover',repeat('a',64),repeat('b',64),
 'cus_waiver','sub_usage_waiver','sub_fee_waiver','si_fee',anchor,start_at,start_at,finish,binding,'{}',clock_timestamp());
 INSERT INTO billing_contract_entitlement_schedule(team_id,contract_id,month_offset,amount_cents)
 SELECT t,c,i,CASE WHEN i<3 THEN 3000000 ELSE 100000 END FROM generate_series(0,11) i;
 INSERT INTO billing_provider_pair_waiver(team_id,contract_id,invoice_id,coupon_id,starts_at,ends_at,evidence,authority_checked_at,authority_valid_until)
 VALUES(t,c,'in_waiver','coupon_fixture',anchor,finish,jsonb_build_object('id','in_waiver','customer','cus_waiver','parent',jsonb_build_object('subscription_details',jsonb_build_object('subscription','sub_fee_waiver')),'status','paid','amount_paid',0,'total',0),clock_timestamp(),clock_timestamp()+interval '50 seconds');
 UPDATE billing_commercial_contract SET status='active',revision=revision+1 WHERE id=c;
 INSERT INTO billing_commercial_offer_activation(assignment_id,team_id,contract_id,merchant_account,live_mode,anchor,ends_at)
 VALUES(a,t,c,'acct_fixture',false,anchor,finish);
 INSERT INTO pair_test_case VALUES(c,t,a,'waiver');
END $$;
