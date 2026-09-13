UPDATE templates
SET subject_template = 'Your doctor application was approved — complete OTP',
    body_template = '<p>Dear Dr. {{.DoctorName}},</p><p>Your application has been approved. Sign in at the doctor portal with your phone number and complete OTP verification to activate your account.</p><p>{{.PortalURL}}</p>'
WHERE key = 'doctor_application_approved' AND channel = 'email' AND locale = 'en';

UPDATE templates
SET subject_template = 'ඔබගේ වෛද්‍ය අයදුම්පත අනුමතයි — OTP සම්පූර්ණ කරන්න',
    body_template = '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>ඔබගේ අයදුම්පත අනුමත කර ඇත. වෛද්‍ය ද්වාරයේ දුරකථන අංකයෙන් පිවිස OTP සම්පූර්ණ කර ගිණුම සක්‍රිය කරන්න.</p><p>{{.PortalURL}}</p>'
WHERE key = 'doctor_application_approved' AND channel = 'email' AND locale = 'si';

UPDATE templates
SET subject_template = 'உங்கள் மருத்துவர் விண்ணப்பம் அங்கீகரிக்கப்பட்டது — OTP முடிக்கவும்',
    body_template = '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>உங்கள் விண்ணப்பம் அங்கீகரிக்கப்பட்டது. மருத்துவர் போர்ட்டலில் தொலைபேசி எண்ணால் உள்நுழைந்து OTP சரிபார்த்து கணக்கை செயல்படுத்தவும்.</p><p>{{.PortalURL}}</p>'
WHERE key = 'doctor_application_approved' AND channel = 'email' AND locale = 'ta';
