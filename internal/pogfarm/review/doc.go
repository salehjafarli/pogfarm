// Package review is the Telegram bot the owner reviews clips through.
//
// Client speaks the Telegram Bot API over HTTPS. Bot receives updates by long
// polling, drops everything that does not come from the owner's chat and
// routes the rest to registered handlers. Only pogfarm runs a Bot: Telegram
// delivers a bot's updates to a single consumer.
//
// Sender is the background loop that posts each clip awaiting review to the
// owner's chat, as a video with Approve and Reject buttons whose callback
// data is "approve:<clip id>" or "reject:<clip id>", and records the message
// on the clip.
package review
