-- Ops inbox email when a doctor applies; applicant emails after approve/reject
-- (before they have a user account — notifications.user_id stores application_id).

INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('doctor_application_submitted', 'email', 'en', 'normal',
 'New doctor application: {{.DoctorName}}',
 '<p>A doctor has applied for verification.</p><ul><li>Name: {{.DoctorName}}</li><li>Email: {{.ApplicantEmail}}</li><li>Phone: {{.Phone}}</li><li>SLMC: {{.SLMCNumber}}</li><li>Specialty: {{.Specialty}}</li></ul><p>Review in the admin console.</p>'),

('doctor_application_approved', 'email', 'en', 'normal',
 'Your doctor application was approved — complete OTP',
 '<p>Dear Dr. {{.DoctorName}},</p><p>Your application has been approved. Sign in at the doctor portal with your phone number and complete OTP verification to activate your account.</p><p>{{.PortalURL}}</p>'),
('doctor_application_approved', 'email', 'si', 'normal',
 'ඔබගේ වෛද්‍ය අයදුම්පත අනුමතයි — OTP සම්පූර්ණ කරන්න',
 '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>ඔබගේ අයදුම්පත අනුමත කර ඇත. වෛද්‍ය ද්වාරයේ දුරකථන අංකයෙන් පිවිස OTP සම්පූර්ණ කර ගිණුම සක්‍රිය කරන්න.</p><p>{{.PortalURL}}</p>'),
('doctor_application_approved', 'email', 'ta', 'normal',
 'உங்கள் மருத்துவர் விண்ணப்பம் அங்கீகரிக்கப்பட்டது — OTP முடிக்கவும்',
 '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>உங்கள் விண்ணப்பம் அங்கீகரிக்கப்பட்டது. மருத்துவர் போர்ட்டலில் தொலைபேசி எண்ணால் உள்நுழைந்து OTP சரிபார்த்து கணக்கை செயல்படுத்தவும்.</p><p>{{.PortalURL}}</p>'),

('doctor_application_rejected', 'email', 'en', 'normal',
 'Your doctor application needs attention',
 '<p>Dear Dr. {{.DoctorName}},</p><p>Your application was not approved this time. Reason: {{.Reason}}</p><p>Please contact us for further details.</p>'),
('doctor_application_rejected', 'email', 'si', 'normal',
 'ඔබගේ වෛද්‍ය අයදුම්පත සම්බන්ධයෙන් අවධානය අවශ්‍යයි',
 '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>ඔබගේ අයදුම්පත මෙවර අනුමත කළ නොහැකි විය. හේතුව: {{.Reason}}</p>'),
('doctor_application_rejected', 'email', 'ta', 'normal',
 'உங்கள் மருத்துவர் விண்ணப்பத்திற்கு கவனம் தேவை',
 '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>உங்கள் விண்ணப்பம் இந்த முறை அங்கீகரிக்கப்படவில்லை. காரணம்: {{.Reason}}</p>');
