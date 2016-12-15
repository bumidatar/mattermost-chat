// Copyright (c) 2016 Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package app

import (
	l4g "github.com/alecthomas/log4go"
	"github.com/mattermost/platform/einterfaces"
	"github.com/mattermost/platform/model"
	"github.com/mattermost/platform/store"
	"github.com/mattermost/platform/utils"
)

func CreatePost(post *model.Post, teamId string, triggerWebhooks bool) (*model.Post, *model.AppError) {
	var pchan store.StoreChannel
	if len(post.RootId) > 0 {
		pchan = Srv.Store.Post().Get(post.RootId)
	}

	// Verify the parent/child relationships are correct
	if pchan != nil {
		if presult := <-pchan; presult.Err != nil {
			return nil, model.NewLocAppError("createPost", "api.post.create_post.root_id.app_error", nil, "")
		} else {
			list := presult.Data.(*model.PostList)
			if len(list.Posts) == 0 || !list.IsChannelId(post.ChannelId) {
				return nil, model.NewLocAppError("createPost", "api.post.create_post.channel_root_id.app_error", nil, "")
			}

			if post.ParentId == "" {
				post.ParentId = post.RootId
			}

			if post.RootId != post.ParentId {
				parent := list.Posts[post.ParentId]
				if parent == nil {
					return nil, model.NewLocAppError("createPost", "api.post.create_post.parent_id.app_error", nil, "")
				}
			}
		}
	}

	post.Hashtags, _ = model.ParseHashtags(post.Message)

	var rpost *model.Post
	if result := <-Srv.Store.Post().Save(post); result.Err != nil {
		return nil, result.Err
	} else {
		rpost = result.Data.(*model.Post)
	}

	if einterfaces.GetMetricsInterface() != nil {
		einterfaces.GetMetricsInterface().IncrementPostCreate()
	}

	if len(post.FileIds) > 0 {
		// There's a rare bug where the client sends up duplicate FileIds so protect against that
		post.FileIds = utils.RemoveDuplicatesFromStringArray(post.FileIds)

		for _, fileId := range post.FileIds {
			if result := <-Srv.Store.FileInfo().AttachToPost(fileId, post.Id); result.Err != nil {
				l4g.Error(utils.T("api.post.create_post.attach_files.error"), post.Id, post.FileIds, post.UserId, result.Err)
			}
		}

		if einterfaces.GetMetricsInterface() != nil {
			einterfaces.GetMetricsInterface().IncrementPostFileAttachment(len(post.FileIds))
		}
	}

	InvalidateCacheForChannelPosts(rpost.ChannelId)

	handlePostEvents(rpost, teamId, triggerWebhooks)

	return rpost, nil
}

func handlePostEvents(post *model.Post, teamId string, triggerWebhooks bool) {
	tchan := Srv.Store.Team().Get(teamId)
	cchan := Srv.Store.Channel().Get(post.ChannelId)
	uchan := Srv.Store.User().Get(post.UserId)

	var team *model.Team
	if result := <-tchan; result.Err != nil {
		l4g.Error(utils.T("api.post.handle_post_events_and_forget.team.error"), teamId, result.Err)
		return
	} else {
		team = result.Data.(*model.Team)
	}

	var channel *model.Channel
	if result := <-cchan; result.Err != nil {
		l4g.Error(utils.T("api.post.handle_post_events_and_forget.channel.error"), post.ChannelId, result.Err)
		return
	} else {
		channel = result.Data.(*model.Channel)
	}

	sendNotifications(c, post, team, channel)

	var user *model.User
	if result := <-uchan; result.Err != nil {
		l4g.Error(utils.T("api.post.handle_post_events_and_forget.user.error"), post.UserId, result.Err)
		return
	} else {
		user = result.Data.(*model.User)
	}

	if triggerWebhooks {
		go handleWebhookEvents(c, post, team, channel, user)
	}

	if channel.Type == model.CHANNEL_DIRECT {
		go makeDirectChannelVisible(post.ChannelId)
	}
}

func SendEphemeralPost(teamId, userId string, post *model.Post) {
	post.Type = model.POST_EPHEMERAL

	// fill in fields which haven't been specified which have sensible defaults
	if post.Id == "" {
		post.Id = model.NewId()
	}
	if post.CreateAt == 0 {
		post.CreateAt = model.GetMillis()
	}
	if post.Props == nil {
		post.Props = model.StringInterface{}
	}

	message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_EPHEMERAL_MESSAGE, "", post.ChannelId, userId, nil)
	message.Add("post", post.ToJson())

	go Publish(message)
}
