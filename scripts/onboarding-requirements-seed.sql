-- Author: Patrick Munis at newwave
-- Per-state stakeholder onboarding requirements matrix (research-driven defaults).
-- check_state_requirements() reads these; admins can amend without redeploys.

-- Texas (state mediation under Ins. Code ch. 1271-1275; TDI-regulated IDREs)
UPDATE public.state_config SET onboarding_requirements = '{
  "IDRE_ENTITY":   ["cms_certification_number","tdi_registration","fee_schedule","coi_attestation","w9","banking_details"],
  "PROVIDER_ORG":  ["npi","tin","w9","texas_medical_board_license"],
  "PAYER_ORG":     ["naic_code","tdi_certificate_of_authority","w9"],
  "STATE_AUDITOR_ORG": ["state_credential_letter"],
  "ADMIN_STAFF":   ["sponsoring_manager","background_check_consent"]
}' WHERE tenant='tx';

-- New York (DFS IDR program)
UPDATE public.state_config SET onboarding_requirements = '{
  "IDRE_ENTITY":   ["cms_certification_number","ny_dfs_approval","fee_schedule","coi_attestation","w9"],
  "PROVIDER_ORG":  ["npi","tin","w9","ny_medical_license"],
  "PAYER_ORG":     ["naic_code","ny_dfs_license","w9"],
  "STATE_AUDITOR_ORG": ["state_credential_letter"],
  "ADMIN_STAFF":   ["sponsoring_manager"]
}' WHERE tenant='ny';

-- Florida / New Jersey / Washington / Maryland (SSL programs)
UPDATE public.state_config SET onboarding_requirements = '{
  "IDRE_ENTITY":   ["cms_certification_number","fee_schedule","coi_attestation","w9"],
  "PROVIDER_ORG":  ["npi","tin","w9","state_medical_license"],
  "PAYER_ORG":     ["naic_code","state_doi_license","w9"],
  "STATE_AUDITOR_ORG": ["state_credential_letter"],
  "ADMIN_STAFF":   ["sponsoring_manager"]
}' WHERE tenant IN ('fl','nj','wa','md');

-- Federal-only states (no SSL program): federal certification suffices
UPDATE public.state_config SET onboarding_requirements = '{
  "IDRE_ENTITY":   ["cms_certification_number","fee_schedule","coi_attestation","w9"],
  "PROVIDER_ORG":  ["npi","tin","w9"],
  "PAYER_ORG":     ["naic_code","w9"],
  "STATE_AUDITOR_ORG": ["state_credential_letter"],
  "ADMIN_STAFF":   ["sponsoring_manager"]
}' WHERE onboarding_requirements = '{}';
