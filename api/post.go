// Copyright (c) 2015 Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package api

import (
	"crypto/tls"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	l4g "github.com/alecthomas/log4go"
	"github.com/gorilla/mux"
	"github.com/mattermost/platform/app"
	"github.com/mattermost/platform/einterfaces"
	"github.com/mattermost/platform/model"
	"github.com/mattermost/platform/store"
	"github.com/mattermost/platform/utils"
	"github.com/nicksnyder/go-i18n/i18n"
)

const (
	TRIGGERWORDS_FULL       = 0
	TRIGGERWORDS_STARTSWITH = 1
)

func InitPost() {
	l4g.Debug(utils.T("api.post.init.debug"))

	BaseRoutes.NeedTeam.Handle("/posts/search", ApiUserRequiredActivity(searchPosts, true)).Methods("POST")
	BaseRoutes.NeedTeam.Handle("/posts/flagged/{offset:[0-9]+}/{limit:[0-9]+}", ApiUserRequired(getFlaggedPosts)).Methods("GET")
	BaseRoutes.NeedTeam.Handle("/posts/{post_id}", ApiUserRequired(getPostById)).Methods("GET")
	BaseRoutes.NeedTeam.Handle("/pltmp/{post_id}", ApiUserRequired(getPermalinkTmp)).Methods("GET")

	BaseRoutes.Posts.Handle("/create", ApiUserRequiredActivity(createPost, true)).Methods("POST")
	BaseRoutes.Posts.Handle("/update", ApiUserRequiredActivity(updatePost, true)).Methods("POST")
	BaseRoutes.Posts.Handle("/page/{offset:[0-9]+}/{limit:[0-9]+}", ApiUserRequired(getPosts)).Methods("GET")
	BaseRoutes.Posts.Handle("/since/{time:[0-9]+}", ApiUserRequired(getPostsSince)).Methods("GET")

	BaseRoutes.NeedPost.Handle("/get", ApiUserRequired(getPost)).Methods("GET")
	BaseRoutes.NeedPost.Handle("/delete", ApiUserRequiredActivity(deletePost, true)).Methods("POST")
	BaseRoutes.NeedPost.Handle("/before/{offset:[0-9]+}/{num_posts:[0-9]+}", ApiUserRequired(getPostsBefore)).Methods("GET")
	BaseRoutes.NeedPost.Handle("/after/{offset:[0-9]+}/{num_posts:[0-9]+}", ApiUserRequired(getPostsAfter)).Methods("GET")
	BaseRoutes.NeedPost.Handle("/get_file_infos", ApiUserRequired(getFileInfosForPost)).Methods("GET")
}

func createPost(c *Context, w http.ResponseWriter, r *http.Request) {
	post := model.PostFromJson(r.Body)
	if post == nil {
		c.SetInvalidParam("createPost", "post")
		return
	}
	post.UserId = c.Session.UserId

	cchan := Srv.Store.Channel().Get(post.ChannelId)

	if !HasPermissionToChannelContext(c, post.ChannelId, model.PERMISSION_CREATE_POST) {
		return
	}

	// Check that channel has not been deleted
	var channel *model.Channel
	if result := <-cchan; result.Err != nil {
		c.SetInvalidParam("createPost", "post.channelId")
		return
	} else {
		channel = result.Data.(*model.Channel)
	}

	if channel.DeleteAt != 0 {
		c.Err = model.NewLocAppError("createPost", "api.post.create_post.can_not_post_to_deleted.error", nil, "")
		c.Err.StatusCode = http.StatusBadRequest
		return
	}

	if post.CreateAt != 0 && !HasPermissionToContext(c, model.PERMISSION_MANAGE_SYSTEM) {
		post.CreateAt = 0
	}

	if rp, err := app.CreatePost(c, post, true); err != nil {
		c.Err = err

		if c.Err.Id == "api.post.create_post.root_id.app_error" ||
			c.Err.Id == "api.post.create_post.channel_root_id.app_error" ||
			c.Err.Id == "api.post.create_post.parent_id.app_error" {
			c.Err.StatusCode = http.StatusBadRequest
		}

		return
	} else {
		// Update the LastViewAt only if the post does not have from_webhook prop set (eg. Zapier app)
		if _, ok := post.Props["from_webhook"]; !ok {
			if result := <-Srv.Store.Channel().UpdateLastViewedAt(post.ChannelId, c.Session.UserId); result.Err != nil {
				l4g.Error(utils.T("api.post.create_post.last_viewed.error"), post.ChannelId, c.Session.UserId, result.Err)
			}
		}

		w.Write([]byte(rp.ToJson()))
	}
}

func makeDirectChannelVisible(channelId string) {
	var members []model.ChannelMember
	if result := <-Srv.Store.Channel().GetMembers(channelId); result.Err != nil {
		l4g.Error(utils.T("api.post.make_direct_channel_visible.get_members.error"), channelId, result.Err.Message)
		return
	} else {
		members = result.Data.([]model.ChannelMember)
	}

	if len(members) != 2 {
		l4g.Error(utils.T("api.post.make_direct_channel_visible.get_2_members.error"), channelId)
		return
	}

	// make sure the channel is visible to both members
	for i, member := range members {
		otherUserId := members[1-i].UserId

		if result := <-Srv.Store.Preference().Get(member.UserId, model.PREFERENCE_CATEGORY_DIRECT_CHANNEL_SHOW, otherUserId); result.Err != nil {
			// create a new preference since one doesn't exist yet
			preference := &model.Preference{
				UserId:   member.UserId,
				Category: model.PREFERENCE_CATEGORY_DIRECT_CHANNEL_SHOW,
				Name:     otherUserId,
				Value:    "true",
			}

			if saveResult := <-Srv.Store.Preference().Save(&model.Preferences{*preference}); saveResult.Err != nil {
				l4g.Error(utils.T("api.post.make_direct_channel_visible.save_pref.error"), member.UserId, otherUserId, saveResult.Err.Message)
			} else {
				message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_PREFERENCE_CHANGED, "", "", member.UserId, nil)
				message.Add("preference", preference.ToJson())

				go Publish(message)
			}
		} else {
			preference := result.Data.(model.Preference)

			if preference.Value != "true" {
				// update the existing preference to make the channel visible
				preference.Value = "true"

				if updateResult := <-Srv.Store.Preference().Save(&model.Preferences{preference}); updateResult.Err != nil {
					l4g.Error(utils.T("api.post.make_direct_channel_visible.update_pref.error"), member.UserId, otherUserId, updateResult.Err.Message)
				} else {
					message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_PREFERENCE_CHANGED, "", "", member.UserId, nil)
					message.Add("preference", preference.ToJson())

					go Publish(message)
				}
			}
		}
	}
}

func updatePost(c *Context, w http.ResponseWriter, r *http.Request) {
	post := model.PostFromJson(r.Body)

	if post == nil {
		c.SetInvalidParam("updatePost", "post")
		return
	}

	pchan := Srv.Store.Post().Get(post.Id)

	if !HasPermissionToChannelContext(c, post.ChannelId, model.PERMISSION_EDIT_POST) {
		return
	}

	var oldPost *model.Post
	if result := <-pchan; result.Err != nil {
		c.Err = result.Err
		return
	} else {
		oldPost = result.Data.(*model.PostList).Posts[post.Id]

		if oldPost == nil {
			c.Err = model.NewLocAppError("updatePost", "api.post.update_post.find.app_error", nil, "id="+post.Id)
			c.Err.StatusCode = http.StatusBadRequest
			return
		}

		if oldPost.UserId != c.Session.UserId {
			c.Err = model.NewLocAppError("updatePost", "api.post.update_post.permissions.app_error", nil, "oldUserId="+oldPost.UserId)
			c.Err.StatusCode = http.StatusForbidden
			return
		}

		if oldPost.DeleteAt != 0 {
			c.Err = model.NewLocAppError("updatePost", "api.post.update_post.permissions.app_error", nil,
				c.T("api.post.update_post.permissions_details.app_error", map[string]interface{}{"PostId": post.Id}))
			c.Err.StatusCode = http.StatusForbidden
			return
		}

		if oldPost.IsSystemMessage() {
			c.Err = model.NewLocAppError("updatePost", "api.post.update_post.system_message.app_error", nil, "id="+post.Id)
			c.Err.StatusCode = http.StatusForbidden
			return
		}
	}

	newPost := &model.Post{}
	*newPost = *oldPost

	newPost.Message = post.Message
	newPost.Hashtags, _ = model.ParseHashtags(post.Message)

	if result := <-Srv.Store.Post().Update(newPost, oldPost); result.Err != nil {
		c.Err = result.Err
		return
	} else {
		rpost := result.Data.(*model.Post)

		message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_POST_EDITED, "", rpost.ChannelId, "", nil)
		message.Add("post", rpost.ToJson())

		go Publish(message)

		InvalidateCacheForChannelPosts(rpost.ChannelId)

		w.Write([]byte(rpost.ToJson()))
	}
}

func getFlaggedPosts(c *Context, w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	offset, err := strconv.Atoi(params["offset"])
	if err != nil {
		c.SetInvalidParam("getFlaggedPosts", "offset")
		return
	}

	limit, err := strconv.Atoi(params["limit"])
	if err != nil {
		c.SetInvalidParam("getFlaggedPosts", "limit")
		return
	}

	posts := &model.PostList{}

	if result := <-Srv.Store.Post().GetFlaggedPosts(c.Session.UserId, offset, limit); result.Err != nil {
		c.Err = result.Err
		return
	} else {
		posts = result.Data.(*model.PostList)
	}

	w.Write([]byte(posts.ToJson()))
}

func getPosts(c *Context, w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	id := params["channel_id"]
	if len(id) != 26 {
		c.SetInvalidParam("getPosts", "channelId")
		return
	}

	offset, err := strconv.Atoi(params["offset"])
	if err != nil {
		c.SetInvalidParam("getPosts", "offset")
		return
	}

	limit, err := strconv.Atoi(params["limit"])
	if err != nil {
		c.SetInvalidParam("getPosts", "limit")
		return
	}

	etagChan := Srv.Store.Post().GetEtag(id, true)

	if !HasPermissionToChannelContext(c, id, model.PERMISSION_CREATE_POST) {
		return
	}

	etag := (<-etagChan).Data.(string)

	if HandleEtag(etag, w, r) {
		return
	}

	pchan := Srv.Store.Post().GetPosts(id, offset, limit)

	if result := <-pchan; result.Err != nil {
		c.Err = result.Err
		return
	} else {
		list := result.Data.(*model.PostList)

		w.Header().Set(model.HEADER_ETAG_SERVER, etag)
		w.Write([]byte(list.ToJson()))
	}

}

func getPostsSince(c *Context, w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	id := params["channel_id"]
	if len(id) != 26 {
		c.SetInvalidParam("getPostsSince", "channelId")
		return
	}

	time, err := strconv.ParseInt(params["time"], 10, 64)
	if err != nil {
		c.SetInvalidParam("getPostsSince", "time")
		return
	}

	pchan := Srv.Store.Post().GetPostsSince(id, time)

	if !HasPermissionToChannelContext(c, id, model.PERMISSION_READ_CHANNEL) {
		return
	}

	if result := <-pchan; result.Err != nil {
		c.Err = result.Err
		return
	} else {
		list := result.Data.(*model.PostList)

		w.Write([]byte(list.ToJson()))
	}

}

func getPost(c *Context, w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	channelId := params["channel_id"]
	if len(channelId) != 26 {
		c.SetInvalidParam("getPost", "channelId")
		return
	}

	postId := params["post_id"]
	if len(postId) != 26 {
		c.SetInvalidParam("getPost", "postId")
		return
	}

	pchan := Srv.Store.Post().Get(postId)

	if !HasPermissionToChannelContext(c, channelId, model.PERMISSION_READ_CHANNEL) {
		return
	}

	if result := <-pchan; result.Err != nil {
		c.Err = result.Err
		return
	} else if HandleEtag(result.Data.(*model.PostList).Etag(), w, r) {
		return
	} else {
		list := result.Data.(*model.PostList)

		if !list.IsChannelId(channelId) {
			c.Err = model.NewLocAppError("getPost", "api.post.get_post.permissions.app_error", nil, "")
			c.Err.StatusCode = http.StatusForbidden
			return
		}

		w.Header().Set(model.HEADER_ETAG_SERVER, list.Etag())
		w.Write([]byte(list.ToJson()))
	}
}

func getPostById(c *Context, w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	postId := params["post_id"]
	if len(postId) != 26 {
		c.SetInvalidParam("getPostById", "postId")
		return
	}

	if result := <-Srv.Store.Post().Get(postId); result.Err != nil {
		c.Err = result.Err
		return
	} else {
		list := result.Data.(*model.PostList)

		if len(list.Order) != 1 {
			c.Err = model.NewLocAppError("getPostById", "api.post_get_post_by_id.get.app_error", nil, "")
			return
		}
		post := list.Posts[list.Order[0]]

		if !HasPermissionToChannelContext(c, post.ChannelId, model.PERMISSION_READ_CHANNEL) {
			return
		}

		if HandleEtag(list.Etag(), w, r) {
			return
		}

		w.Header().Set(model.HEADER_ETAG_SERVER, list.Etag())
		w.Write([]byte(list.ToJson()))
	}
}

func getPermalinkTmp(c *Context, w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	postId := params["post_id"]
	if len(postId) != 26 {
		c.SetInvalidParam("getPermalinkTmp", "postId")
		return
	}

	if result := <-Srv.Store.Post().Get(postId); result.Err != nil {
		c.Err = result.Err
		return
	} else {
		list := result.Data.(*model.PostList)

		if len(list.Order) != 1 {
			c.Err = model.NewLocAppError("getPermalinkTmp", "api.post_get_post_by_id.get.app_error", nil, "")
			return
		}
		post := list.Posts[list.Order[0]]

		// Because we confuse permissions and membership in Mattermost's model, we have to just
		// try to join the channel without checking if we already have permission to it. This is
		// because system admins have permissions to every channel but are not nessisary a member
		// of every channel. If we checked here then system admins would skip joining the channel and
		// error when they tried to view it.
		if err, _ := JoinChannelById(c, c.Session.UserId, post.ChannelId); err != nil {
			// On error just return with permissions error
			c.Err = err
			return
		}

		if HandleEtag(list.Etag(), w, r) {
			return
		}

		w.Header().Set(model.HEADER_ETAG_SERVER, list.Etag())
		w.Write([]byte(list.ToJson()))
	}
}

func deletePost(c *Context, w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	channelId := params["channel_id"]
	if len(channelId) != 26 {
		c.SetInvalidParam("deletePost", "channelId")
		return
	}

	postId := params["post_id"]
	if len(postId) != 26 {
		c.SetInvalidParam("deletePost", "postId")
		return
	}

	if !HasPermissionToChannelContext(c, channelId, model.PERMISSION_EDIT_POST) {
		return
	}

	pchan := Srv.Store.Post().Get(postId)

	if result := <-pchan; result.Err != nil {
		c.Err = result.Err
		return
	} else {

		post := result.Data.(*model.PostList).Posts[postId]

		if post == nil {
			c.SetInvalidParam("deletePost", "postId")
			return
		}

		if post.ChannelId != channelId {
			c.Err = model.NewLocAppError("deletePost", "api.post.delete_post.permissions.app_error", nil, "")
			c.Err.StatusCode = http.StatusForbidden
			return
		}

		if post.UserId != c.Session.UserId && !HasPermissionToChannelContext(c, post.ChannelId, model.PERMISSION_EDIT_OTHERS_POSTS) {
			c.Err = model.NewLocAppError("deletePost", "api.post.delete_post.permissions.app_error", nil, "")
			c.Err.StatusCode = http.StatusForbidden
			return
		}

		if dresult := <-Srv.Store.Post().Delete(postId, model.GetMillis()); dresult.Err != nil {
			c.Err = dresult.Err
			return
		}

		message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_POST_DELETED, "", post.ChannelId, "", nil)
		message.Add("post", post.ToJson())

		go Publish(message)
		go DeletePostFiles(post)
		go DeleteFlaggedPost(c.Session.UserId, post)

		InvalidateCacheForChannelPosts(post.ChannelId)

		result := make(map[string]string)
		result["id"] = postId
		w.Write([]byte(model.MapToJson(result)))
	}
}

func DeleteFlaggedPost(userId string, post *model.Post) {
	if result := <-Srv.Store.Preference().Delete(userId, model.PREFERENCE_CATEGORY_FLAGGED_POST, post.Id); result.Err != nil {
		l4g.Warn(utils.T("api.post.delete_flagged_post.app_error.warn"), result.Err)
		return
	}
}

func DeletePostFiles(post *model.Post) {
	if len(post.FileIds) != 0 {
		return
	}

	if result := <-Srv.Store.FileInfo().DeleteForPost(post.Id); result.Err != nil {
		l4g.Warn(utils.T("api.post.delete_post_files.app_error.warn"), post.Id, result.Err)
	}
}

func getPostsBefore(c *Context, w http.ResponseWriter, r *http.Request) {
	getPostsBeforeOrAfter(c, w, r, true)
}

func getPostsAfter(c *Context, w http.ResponseWriter, r *http.Request) {
	getPostsBeforeOrAfter(c, w, r, false)
}

func getPostsBeforeOrAfter(c *Context, w http.ResponseWriter, r *http.Request, before bool) {
	params := mux.Vars(r)

	id := params["channel_id"]
	if len(id) != 26 {
		c.SetInvalidParam("getPostsBeforeOrAfter", "channelId")
		return
	}

	postId := params["post_id"]
	if len(postId) != 26 {
		c.SetInvalidParam("getPostsBeforeOrAfter", "postId")
		return
	}

	numPosts, err := strconv.Atoi(params["num_posts"])
	if err != nil || numPosts <= 0 {
		c.SetInvalidParam("getPostsBeforeOrAfter", "numPosts")
		return
	}

	offset, err := strconv.Atoi(params["offset"])
	if err != nil || offset < 0 {
		c.SetInvalidParam("getPostsBeforeOrAfter", "offset")
		return
	}

	// We can do better than this etag in this situation
	etagChan := Srv.Store.Post().GetEtag(id, true)

	if !HasPermissionToChannelContext(c, id, model.PERMISSION_READ_CHANNEL) {
		return
	}

	etag := (<-etagChan).Data.(string)
	if HandleEtag(etag, w, r) {
		return
	}

	var pchan store.StoreChannel
	if before {
		pchan = Srv.Store.Post().GetPostsBefore(id, postId, numPosts, offset)
	} else {
		pchan = Srv.Store.Post().GetPostsAfter(id, postId, numPosts, offset)
	}

	if result := <-pchan; result.Err != nil {
		c.Err = result.Err
		return
	} else {
		list := result.Data.(*model.PostList)

		w.Header().Set(model.HEADER_ETAG_SERVER, etag)
		w.Write([]byte(list.ToJson()))
	}
}

func searchPosts(c *Context, w http.ResponseWriter, r *http.Request) {
	props := model.StringInterfaceFromJson(r.Body)

	terms := props["terms"].(string)
	if len(terms) == 0 {
		c.SetInvalidParam("search", "terms")
		return
	}

	isOrSearch := false
	if val, ok := props["is_or_search"]; ok && val != nil {
		isOrSearch = val.(bool)
	}

	paramsList := model.ParseSearchParams(terms)
	channels := []store.StoreChannel{}

	for _, params := range paramsList {
		params.OrTerms = isOrSearch
		// don't allow users to search for everything
		if params.Terms != "*" {
			channels = append(channels, Srv.Store.Post().Search(c.TeamId, c.Session.UserId, params))
		}
	}

	posts := &model.PostList{}
	for _, channel := range channels {
		if result := <-channel; result.Err != nil {
			c.Err = result.Err
			return
		} else {
			data := result.Data.(*model.PostList)
			posts.Extend(data)
		}
	}

	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write([]byte(posts.ToJson()))
}

func getFileInfosForPost(c *Context, w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	channelId := params["channel_id"]
	if len(channelId) != 26 {
		c.SetInvalidParam("getFileInfosForPost", "channelId")
		return
	}

	postId := params["post_id"]
	if len(postId) != 26 {
		c.SetInvalidParam("getFileInfosForPost", "postId")
		return
	}

	pchan := Srv.Store.Post().Get(postId)
	fchan := Srv.Store.FileInfo().GetForPost(postId)

	if !HasPermissionToChannelContext(c, channelId, model.PERMISSION_READ_CHANNEL) {
		return
	}

	var infos []*model.FileInfo
	if result := <-fchan; result.Err != nil {
		c.Err = result.Err
		return
	} else {
		infos = result.Data.([]*model.FileInfo)
	}

	if len(infos) == 0 {
		// No FileInfos were returned so check if they need to be created for this post
		var post *model.Post
		if result := <-pchan; result.Err != nil {
			c.Err = result.Err
			return
		} else {
			post = result.Data.(*model.PostList).Posts[postId]
		}

		if len(post.Filenames) > 0 {
			// The post has Filenames that need to be replaced with FileInfos
			infos = migrateFilenamesToFileInfos(post)
		}
	}

	etag := model.GetEtagForFileInfos(infos)

	if HandleEtag(etag, w, r) {
		return
	} else {
		w.Header().Set("Cache-Control", "max-age=2592000, public")
		w.Header().Set(model.HEADER_ETAG_SERVER, etag)
		w.Write([]byte(model.FileInfosToJson(infos)))
	}
}
