DELETE FROM templates WHERE key IN (
    'booking_confirmed', 'reminder_1h', 'reminder_24h', 'doctor_approved',
    'doctor_rejected', 'prescription_ready', 'payment_receipt', 'payment_failed',
    'payout_sent', 'waitlist_available', 'consultation_starting',
    'appointment_cancelled', 'otp_code'
);
