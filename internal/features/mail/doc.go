// Package mail owns Nano's durable inbox policy above the interact boundary.
// It also owns the count-change publisher shared by its HTTP and sidebar
// consumers. It depends on interact; core prompt admission may consume mail's generic
// notice without depending on mail storage.
package mail
