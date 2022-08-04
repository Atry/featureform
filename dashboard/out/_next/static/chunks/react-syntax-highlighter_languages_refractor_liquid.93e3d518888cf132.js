"use strict";(self.webpackChunk_N_E=self.webpackChunk_N_E||[]).push([[1323,3047],{34927:function(a,b,c){var d=c(93205);function e(a){a.register(d),a.languages.liquid={comment:{pattern:/(^\{%\s*comment\s*%\})[\s\S]+(?=\{%\s*endcomment\s*%\}$)/,lookbehind:!0},delimiter:{pattern:/^\{(?:\{\{|[%\{])-?|-?(?:\}\}|[%\}])\}$/,alias:"punctuation"},string:{pattern:/"[^"]*"|'[^']*'/,greedy:!0},keyword:/\b(?:as|assign|break|(?:end)?(?:capture|case|comment|for|form|if|paginate|raw|style|tablerow|unless)|continue|cycle|decrement|echo|else|elsif|in|include|increment|limit|liquid|offset|range|render|reversed|section|when|with)\b/,object:/\b(?:address|all_country_option_tags|article|block|blog|cart|checkout|collection|color|country|country_option_tags|currency|current_page|current_tags|customer|customer_address|date|discount_allocation|discount_application|external_video|filter|filter_value|font|forloop|fulfillment|generic_file|gift_card|group|handle|image|line_item|link|linklist|localization|location|measurement|media|metafield|model|model_source|order|page|page_description|page_image|page_title|part|policy|product|product_option|recommendations|request|robots|routes|rule|script|search|selling_plan|selling_plan_allocation|selling_plan_group|shipping_method|shop|shop_locale|sitemap|store_availability|tax_line|template|theme|transaction|unit_price_measurement|user_agent|variant|video|video_source)\b/,function:[{pattern:/(\|\s*)\w+/,lookbehind:!0,alias:"filter"},{pattern:/(\.\s*)(?:first|last|size)/,lookbehind:!0}],boolean:/\b(?:false|nil|true)\b/,range:{pattern:/\.\./,alias:"operator"},number:/\b\d+(?:\.\d+)?\b/,operator:/[!=]=|<>|[<>]=?|[|?:=-]|\b(?:and|contains(?=\s)|or)\b/,punctuation:/[.,\[\]()]/,empty:{pattern:/\bempty\b/,alias:"keyword"}},a.hooks.add("before-tokenize",function(b){var c=!1;a.languages["markup-templating"].buildPlaceholders(b,"liquid",/\{%\s*comment\s*%\}[\s\S]*?\{%\s*endcomment\s*%\}|\{(?:%[\s\S]*?%|\{\{[\s\S]*?\}\}|\{[\s\S]*?\})\}/g,function(a){var b=/^\{%-?\s*(\w+)/.exec(a);if(b){var d=b[1];if("raw"===d&&!c)return c=!0,!0;if("endraw"===d)return c=!1,!0}return!c})}),a.hooks.add("after-tokenize",function(b){a.languages["markup-templating"].tokenizePlaceholders(b,"liquid")})}a.exports=e,e.displayName="liquid",e.aliases=[]},93205:function(a){function b(a){!function(a){function b(a,b){return"___"+a.toUpperCase()+b+"___"}Object.defineProperties(a.languages["markup-templating"]={},{buildPlaceholders:{value:function(c,d,e,f){if(c.language===d){var g=c.tokenStack=[];c.code=c.code.replace(e,function(a){if("function"==typeof f&&!f(a))return a;for(var e,h=g.length;-1!==c.code.indexOf(e=b(d,h));)++h;return g[h]=a,e}),c.grammar=a.languages.markup}}},tokenizePlaceholders:{value:function(c,d){if(c.language===d&&c.tokenStack){c.grammar=a.languages[d];var e=0,f=Object.keys(c.tokenStack);g(c.tokens)}function g(h){for(var i=0;i<h.length&&!(e>=f.length);i++){var j=h[i];if("string"==typeof j||j.content&&"string"==typeof j.content){var k=f[e],l=c.tokenStack[k],m="string"==typeof j?j:j.content,n=b(d,k),o=m.indexOf(n);if(o> -1){++e;var p=m.substring(0,o),q=new a.Token(d,a.tokenize(l,c.grammar),"language-"+d,l),r=m.substring(o+n.length),s=[];p&&s.push.apply(s,g([p])),s.push(q),r&&s.push.apply(s,g([r])),"string"==typeof j?h.splice.apply(h,[i,1].concat(s)):j.content=s}}else j.content&&g(j.content)}return h}}}})}(a)}a.exports=b,b.displayName="markupTemplating",b.aliases=[]}}])